package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	eksv1alpha1 "github.com/aws-controllers-k8s/eks-controller/apis/v1alpha1"
	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	"github.com/go-logr/logr"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

// AWSServiceAccountRoleReconciler reconciles workload service account role bindings.
//
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles/finalizers,verbs=update
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityoperatorconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=iam.services.k8s.aws,resources=roles;policies;openidconnectproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eks.services.k8s.aws,resources=podidentityassociations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type AWSServiceAccountRoleReconciler struct {
	client.Client
	MCManager               mcmanager.Manager
	Scheme                  *runtime.Scheme
	Recorder                events.EventRecorder
	Resolver                inventory.Resolver
	MaxConcurrentReconciles int
	RoleEnqueueChannel      <-chan crevent.TypedGenericEvent[*identityv1.AWSServiceAccountRole]
}

// Reconcile drives the hub IAM resources (Role/Policy/PodIdentityAssociation)
// for an AWSServiceAccountRole and propagates the resulting ARN onto the
// referenced ServiceAccount on the workload cluster.
func (r *AWSServiceAccountRoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	log := loggerForRequest(ctx, metrics.ControllerRole, req)
	ctx = logf.IntoContext(ctx, log)
	log.V(1).Info("starting reconcile")

	defer func() {
		logReconcileEnd(log, result, reconcileErr)
	}()

	role := &identityv1.AWSServiceAccountRole{}
	if err := r.Get(ctx, req.NamespacedName, role); err != nil {
		if ignored := client.IgnoreNotFound(err); ignored != nil {
			return ctrl.Result{}, fmt.Errorf("get AWSServiceAccountRole %s: %w", req.NamespacedName, ignored)
		}

		return ctrl.Result{}, nil
	}

	added, err := ensureFinalizer(ctx, r.Client, r.Recorder, log, role, identityv1.ServiceAccountRoleFinalizer)
	if err != nil {
		return ctrl.Result{}, err
	}

	if added {
		return ctrl.Result{}, nil
	}

	if !role.DeletionTimestamp.IsZero() {
		if err := r.reconcileDelete(ctx, role); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	return r.reconcileNormal(ctx, role)
}

type roleReconcileInputs struct {
	operatorConfig *identityv1.AWSWorkloadIdentityOperatorConfig
	config         *identityv1.AWSWorkloadIdentityConfig
	resolved       inventory.Resolution
	trustPolicy    string
}

func (r *AWSServiceAccountRoleReconciler) reconcileNormal(ctx context.Context, role *identityv1.AWSServiceAccountRole) (ctrl.Result, error) {
	beforeStatus := role.Status.DeepCopy()
	log := logf.FromContext(ctx)

	role.Status.ObservedGeneration = role.Generation

	if result, done, err := r.reconcileDuplicateBindingConflict(ctx, log, role, beforeStatus); done || err != nil {
		return result, err
	}

	inputs, result, err := r.prepareRoleInputs(ctx, log, role, beforeStatus)
	if errors.Is(err, errReconcileDone) {
		return result, nil
	}

	if err != nil {
		return result, err
	}

	delivery := inputs.config.Spec.Type
	log = log.WithValues("awio.delivery.type", string(delivery))
	ctx = logf.IntoContext(ctx, log)

	setRoleResolvedDeliveryStatus(role, delivery, &inputs.resolved)
	role.Status.ACKResources = make([]identityv1.ACKResourceStatus, 0, 3)

	policyARNs, err := r.reconcileGeneratedPolicy(ctx, log, role, delivery, role.Spec.PolicyARNs)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileIAMRole(ctx, log, role, inputs, policyARNs); err != nil {
		return ctrl.Result{}, err
	}

	if shouldPersistSelfHostedDeliveryContextBeforeRemotePatch(role, beforeStatus, delivery) {
		message := "waiting to retry remote ServiceAccount annotation delivery after persisting role status"
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, message)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, message)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, message)

		if err := r.patchRoleStatus(ctx, log, role, beforeStatus); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	result, err = r.setDeliveryConditions(ctx, log, role, delivery, inputs, beforeStatus)
	if err != nil {
		return ctrl.Result{}, err
	}

	setHubReadyConditions(role, delivery, inputs.config)

	if err := r.patchRoleStatus(ctx, log, role, beforeStatus); err != nil {
		return ctrl.Result{}, err
	}

	return roleResultWithSelfHostedSafetyRequeue(delivery, role, result), nil
}

func shouldPersistSelfHostedDeliveryContextBeforeRemotePatch(role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus, delivery identityv1.DeliveryType) bool {
	return delivery == identityv1.DeliveryTypeSelfHostedIRSA &&
		role.Status.RoleARN != "" &&
		(beforeStatus.RoleARN != role.Status.RoleARN ||
			beforeStatus.DeliveryType != role.Status.DeliveryType ||
			beforeStatus.ResolvedClusterName != role.Status.ResolvedClusterName)
}

func (r *AWSServiceAccountRoleReconciler) reconcileDuplicateBindingConflict(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus) (ctrl.Result, bool, error) {
	conflicts, err := r.conflictingServiceAccountBindingNames(ctx, role)
	if err != nil {
		return ctrl.Result{}, true, err
	}

	if len(conflicts) < 2 {
		return ctrl.Result{}, false, nil
	}

	message := fmt.Sprintf("service account %s/%s is bound by multiple AWSServiceAccountRole objects: %v",
		role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name, conflicts)
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec, message)
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec, message)

	if err := r.patchRoleStatus(ctx, log, role, beforeStatus); err != nil {
		return ctrl.Result{}, true, err
	}

	return ctrl.Result{RequeueAfter: transientRequeue}, true, nil
}

func (r *AWSServiceAccountRoleReconciler) conflictingServiceAccountBindingNames(ctx context.Context, role *identityv1.AWSServiceAccountRole) ([]string, error) {
	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := r.List(ctx, roles, client.InNamespace(role.Namespace), roleByServiceAccountKey(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name)); err != nil {
		return nil, fmt.Errorf("list AWSServiceAccountRoles in namespace %q: %w", role.Namespace, err)
	}

	conflicts := make([]string, 0, len(roles.Items)+1)
	selfFound := false

	for i := range roles.Items {
		existing := &roles.Items[i]
		if !existing.DeletionTimestamp.IsZero() {
			continue
		}

		conflicts = append(conflicts, client.ObjectKeyFromObject(existing).String())
		if existing.UID == role.UID {
			selfFound = true
		}
	}

	if !selfFound {
		conflicts = append(conflicts, client.ObjectKeyFromObject(role).String())
	}

	slices.Sort(conflicts)

	return conflicts, nil
}

// prepareRoleInputs gathers operator config, namespace config, inventory
// resolution, and trust policy for the role reconcile. A returned
// errReconcileDone (joined) means the helper has already produced the final
// ctrl.Result and patched status; the caller should return it without further
// work.
func (r *AWSServiceAccountRoleReconciler) prepareRoleInputs(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus) (*roleReconcileInputs, ctrl.Result, error) {
	operatorConfig, err := loadOperatorConfig(ctx, r.Client)
	if err != nil {
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionOperatorConfigReady, identityv1.ReasonOperatorConfigUnavailable, err.Error())
		log.V(1).Info("operator configuration unavailable", "awio.operation", "load_operator_config")

		if patchErr := r.patchRoleStatus(ctx, log, role, beforeStatus); patchErr != nil {
			return nil, ctrl.Result{}, patchErr
		}

		return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionOperatorConfigReady, metav1.ConditionTrue, identityv1.ReasonReady, "operator configuration is valid")

	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: identityv1.DefaultName}, config); err != nil {
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionConfigResolved, identityv1.ReasonConfigUnavailable, err.Error())

		if patchErr := r.patchRoleStatus(ctx, log, role, beforeStatus); patchErr != nil {
			return nil, ctrl.Result{}, patchErr
		}

		return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionConfigResolved, metav1.ConditionTrue, identityv1.ReasonResolved, "namespace config resolved")

	resolved, result, err := r.resolveInventory(ctx, log, role, beforeStatus)
	if err != nil {
		return nil, result, err
	}

	trustPolicy, err := r.trustPolicy(role, config, &resolved)
	if err != nil {
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionTrustPolicyReady, identityv1.ReasonTrustPolicyInputMissing, err.Error())

		if patchErr := r.patchRoleStatus(ctx, log, role, beforeStatus); patchErr != nil {
			return nil, ctrl.Result{}, patchErr
		}

		return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionTrustPolicyReady, metav1.ConditionTrue, identityv1.ReasonRendered, "trust policy rendered")

	return &roleReconcileInputs{
		operatorConfig: operatorConfig,
		config:         config,
		resolved:       resolved,
		trustPolicy:    trustPolicy,
	}, ctrl.Result{}, nil
}

func (r *AWSServiceAccountRoleReconciler) resolveInventory(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus) (inventory.Resolution, ctrl.Result, error) {
	resolved, err := r.Resolver.Resolve(ctx, role.Namespace)
	if err != nil {
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionInventoryResolved, identityv1.ReasonResolverError, err.Error())

		if patchErr := r.patchRoleStatus(ctx, log, role, beforeStatus); patchErr != nil {
			log.Error(patchErr, "failed to patch status after inventory resolver error")
		}

		return inventory.Resolution{}, ctrl.Result{}, fmt.Errorf("resolve inventory for namespace %q: %w", role.Namespace, err)
	}

	if !resolved.Ready {
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionInventoryResolved, metav1.ConditionFalse, resolved.Reason, resolved.Message)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, resolved.Reason, resolved.Message)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionReady, metav1.ConditionFalse, resolved.Reason, resolved.Message)
		log.V(1).Info("waiting for inventory resolution", "awio.condition.reason", resolved.Reason)

		if patchErr := r.patchRoleStatus(ctx, log, role, beforeStatus); patchErr != nil {
			return inventory.Resolution{}, ctrl.Result{}, patchErr
		}

		return inventory.Resolution{}, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionInventoryResolved, metav1.ConditionTrue, resolved.Reason, resolved.Message)

	return resolved, ctrl.Result{}, nil
}

func setRoleResolvedDeliveryStatus(role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, resolved *inventory.Resolution) {
	role.Status.DeliveryType = delivery
	role.Status.ResolvedClusterName = ""

	if delivery == identityv1.DeliveryTypeSelfHostedIRSA && resolved != nil && resolved.Ready {
		role.Status.ResolvedClusterName = resolved.ClusterName.String()
	}
}

func (r *AWSServiceAccountRoleReconciler) reconcileIAMRole(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, inputs *roleReconcileInputs, policyARNs []string) error {
	desired := identityaws.BuildIAMRole(role, inputs.config.Spec.Type, inputs.operatorConfig.Spec.PermissionsBoundaryARN, inputs.trustPolicy, policyARNs)
	current := &iamv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	op, err := createOrUpdate(ctx, r.Client, r.Scheme, role, current, func() error {
		current.Labels = desired.Labels
		// ACK iamv1alpha1.RoleSpec carries no status-bearing fields (status lives in
		// RoleStatus), so wholesale Spec assignment cannot clobber controller-set state
		// and stays correct if BuildIAMRole later populates more fields.
		current.Spec = desired.Spec

		return nil
	})
	logChildApply(log, metrics.ControllerRole, ackChildKindRole, current.Name, op, err)

	if err != nil {
		return err
	}

	role.Status.ACKResources = append(role.Status.ACKResources, identityaws.ACKResourceStatus(iamv1alpha1.GroupVersion.String(), ackChildKindRole, current, current.Status.Conditions))
	role.Status.RoleARN = identityaws.ARN(current.Status.ACKResourceMetadata)
	setACKReadyCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionRoleReady, "IAM Role", identityaws.IsACKSynced(current.Status.Conditions) && role.Status.RoleARN != "")

	return nil
}

// setHubReadyConditions writes DeliveryReady and Ready in lockstep. Both
// conditions always carry the same status/reason — Ready is the sum of hub ACK
// resources plus delivery-specific signals, and DeliveryReady is the same view
// reported separately for downstream consumers.
func setHubReadyConditions(role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, config *identityv1.AWSWorkloadIdentityConfig) {
	status, reason, message := computeRoleReadyState(role, delivery, config)
	readyReason := reason

	if status == metav1.ConditionTrue {
		readyReason = identityv1.ReasonReady
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, status, reason, message)
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionReady, status, readyReason, message)
}

func computeRoleReadyState(role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, config *identityv1.AWSWorkloadIdentityConfig) (metav1.ConditionStatus, string, string) {
	if !hubRoleResourcesReady(role) {
		return metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for ACK resources"
	}

	switch delivery {
	case identityv1.DeliveryTypeSelfHostedIRSA:
		if !meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionServiceAccountAnnotationReady) {
			return metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, "waiting for remote ServiceAccount annotations"
		}

		if !meta.IsStatusConditionTrue(config.Status.Conditions, identityv1.ConditionReady) {
			return metav1.ConditionFalse, identityv1.ReasonConfigNotReady, fmt.Sprintf("waiting for AWSWorkloadIdentityConfig/%s/%s to become Ready", config.Namespace, config.Name)
		}
	case identityv1.DeliveryTypeEKSPodIdentity:
		if !meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionPodIdentityAssocReady) ||
			!meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionPodIdentityAgentReady) {
			return metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for ACK resources"
		}
	}

	return metav1.ConditionTrue, identityv1.ReasonHubResourcesReady, "role delivery resources are ready; remote runtime status is reported separately"
}

func hubRoleResourcesReady(role *identityv1.AWSServiceAccountRole) bool {
	return meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionRoleReady) &&
		meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionPolicyReady)
}

func (r *AWSServiceAccountRoleReconciler) setDeliveryConditions(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, inputs *roleReconcileInputs, beforeStatus *identityv1.AWSServiceAccountRoleStatus) (ctrl.Result, error) {
	if delivery == identityv1.DeliveryTypeEKSPodIdentity {
		if err := r.reconcilePodIdentityAssociation(ctx, log, role, &inputs.resolved); err != nil {
			return ctrl.Result{}, err
		}

		setPodIdentityAgentCondition(role, &inputs.resolved)

		return ctrl.Result{}, nil
	}

	if role.Status.RoleARN == "" {
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for IAM Role ARN before patching the remote ServiceAccount")

		return ctrl.Result{}, nil
	}

	saPatchOp, err := r.reconcileSelfHostedDelivery(ctx, log, role, inputs)
	if err != nil {
		// Log explicitly: returning nil below suppresses controller-runtime's own error log.
		log.Error(err, "self-hosted delivery deferred", "awio.condition.reason", identityv1.ReasonRemoteDeliveryPending)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, err.Error())

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	annotationReason := serviceAccountAnnotationSyncReason(r.Recorder, role, beforeStatus, saPatchOp)

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, annotationReason, "remote ServiceAccount annotations are synced")

	return ctrl.Result{}, nil
}

func roleResultWithSelfHostedSafetyRequeue(delivery identityv1.DeliveryType, role *identityv1.AWSServiceAccountRole, result ctrl.Result) ctrl.Result {
	if delivery == identityv1.DeliveryTypeSelfHostedIRSA &&
		result.IsZero() &&
		meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionServiceAccountAnnotationReady) {
		return ctrl.Result{RequeueAfter: selfHostedSteadyStateRequeue}
	}

	if result.IsZero() {
		return ctrl.Result{RequeueAfter: dependencySteadyStateRequeue}
	}

	return result
}

func serviceAccountAnnotationSyncReason(recorder events.EventRecorder, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus, patchOp controllerutil.OperationResult) string {
	if beforeStatus != nil &&
		meta.IsStatusConditionTrue(beforeStatus.Conditions, identityv1.ConditionServiceAccountAnnotationReady) &&
		patchOp == controllerutil.OperationResultUpdated {
		recordAnnotationRepaired(recorder, role)

		return identityv1.ReasonAnnotationRepaired
	}

	return identityv1.ReasonReady
}

func (r *AWSServiceAccountRoleReconciler) reconcileSelfHostedDelivery(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, inputs *roleReconcileInputs) (controllerutil.OperationResult, error) {
	target, err := remoteClusterClient(ctx, r.MCManager, &inputs.resolved)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	op, err := patchRemoteServiceAccountAnnotations(ctx, target, role.Spec.ServiceAccount, role.Status.RoleARN)
	logRemoteApply(log, metrics.ResourceServiceAccount, serviceAccountIndexKey(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name), op, err)

	return op, err
}

func (r *AWSServiceAccountRoleReconciler) reconcileGeneratedPolicy(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, policyARNs []string) ([]string, error) {
	if role.Spec.PolicyDocument == nil || len(role.Spec.PolicyDocument.Raw) == 0 {
		policy := &iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace}}
		if err := client.IgnoreNotFound(r.Delete(ctx, policy)); err != nil {
			return nil, fmt.Errorf("delete generated IAM Policy %s/%s: %w", policy.Namespace, policy.Name, err)
		}

		role.Status.GeneratedPolicyARN = ""
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, metav1.ConditionTrue, identityv1.ReasonManagedPoliciesOnly, "managed policy ARNs are allowed")

		return policyARNs, nil
	}

	policyDoc, err := identityaws.PolicyDocumentString(role.Spec.PolicyDocument.Raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize policy document: %w", err)
	}

	desired := identityaws.BuildIAMPolicy(role, delivery, policyDoc)
	current := &iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	op, err := createOrUpdate(ctx, r.Client, r.Scheme, role, current, func() error {
		current.Labels = desired.Labels
		current.Spec = desired.Spec

		return nil
	})
	logChildApply(log, metrics.ControllerRole, ackChildKindPolicy, current.Name, op, err)

	if err != nil {
		return nil, fmt.Errorf("apply IAM Policy %s/%s: %w", current.Namespace, current.Name, err)
	}

	role.Status.ACKResources = append(role.Status.ACKResources, identityaws.ACKResourceStatus(iamv1alpha1.GroupVersion.String(), ackChildKindPolicy, current, current.Status.Conditions))
	role.Status.GeneratedPolicyARN = identityaws.ARN(current.Status.ACKResourceMetadata)

	if role.Status.GeneratedPolicyARN != "" {
		// Clone before append so we never mutate the caller's spec-backed slice
		// when the underlying array still has capacity.
		policyARNs = append(slices.Clone(policyARNs), role.Status.GeneratedPolicyARN)
	}

	setACKReadyCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, "generated IAM Policy", identityaws.IsACKSynced(current.Status.Conditions))

	return policyARNs, nil
}

func (r *AWSServiceAccountRoleReconciler) reconcilePodIdentityAssociation(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, resolved *inventory.Resolution) error {
	if role.Status.RoleARN == "" {
		return nil
	}

	desired := identityaws.BuildPodIdentityAssociation(role, role.Status.RoleARN, identityaws.EKSIdentity{
		ClusterName:       resolved.EKSClusterName,
		ClusterARN:        resolved.EKSClusterARN,
		AWSOrganizationID: resolved.AWSOrganizationID,
	})
	current := &eksv1alpha1.PodIdentityAssociation{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	op, err := createOrUpdate(ctx, r.Client, r.Scheme, role, current, func() error {
		current.Labels = desired.Labels
		current.Spec = desired.Spec

		return nil
	})
	logChildApply(log, metrics.ControllerRole, ackChildKindPodIdentityAssociation, current.Name, op, err)

	if err != nil {
		return fmt.Errorf("apply PodIdentityAssociation %s/%s: %w", current.Namespace, current.Name, err)
	}

	role.Status.ACKResources = append(role.Status.ACKResources, identityaws.ACKResourceStatus(eksv1alpha1.GroupVersion.String(), ackChildKindPodIdentityAssociation, current, current.Status.Conditions))

	setACKReadyCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAssocReady, "EKS PodIdentityAssociation", identityaws.IsACKSynced(current.Status.Conditions))

	return nil
}

func setPodIdentityAgentCondition(role *identityv1.AWSServiceAccountRole, resolved *inventory.Resolution) {
	if resolved.EKSAutoMode {
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAgentReady, metav1.ConditionTrue, identityv1.ReasonEKSAutoMode, "ClusterProfile declares EKS Auto Mode")

		return
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAgentReady, metav1.ConditionUnknown, identityv1.ReasonRemoteCheckPending, "remote EKS Pod Identity Agent readiness is checked by the remote runtime controller")
}

func (r *AWSServiceAccountRoleReconciler) trustPolicy(role *identityv1.AWSServiceAccountRole, config *identityv1.AWSWorkloadIdentityConfig, resolved *inventory.Resolution) (string, error) {
	switch config.Spec.Type {
	case identityv1.DeliveryTypeSelfHostedIRSA:
		if config.Status.OIDCProviderARN == "" {
			return "", fmt.Errorf("AWSWorkloadIdentityConfig.status.oidcProviderARN is empty")
		}

		if config.Status.IssuerHostPath == "" {
			return "", fmt.Errorf("AWSWorkloadIdentityConfig.status.issuerHostPath is empty")
		}

		policy, err := identityaws.SelfHostedTrustPolicy(config.Status.IssuerHostPath, config.Status.OIDCProviderARN, role.Spec.ServiceAccount)
		if err != nil {
			return "", fmt.Errorf("build self-hosted trust policy: %w", err)
		}

		return policy, nil
	case identityv1.DeliveryTypeEKSPodIdentity:
		if err := resolved.RequireEKS(); err != nil {
			return "", fmt.Errorf("require EKS inventory properties: %w", err)
		}

		policy, err := identityaws.EKSPodIdentityTrustPolicy(resolved.EKSClusterARN, resolved.AWSOrganizationID, role.Spec.ServiceAccount)
		if err != nil {
			return "", fmt.Errorf("build EKS Pod Identity trust policy: %w", err)
		}

		return policy, nil
	default:
		return "", fmt.Errorf("unsupported delivery type %q", config.Spec.Type)
	}
}

func (r *AWSServiceAccountRoleReconciler) reconcileDelete(ctx context.Context, role *identityv1.AWSServiceAccountRole) error {
	log := logf.FromContext(ctx)

	if err := r.cleanupRemoteServiceAccountAnnotations(ctx, log, role); err != nil {
		return err
	}

	deletes := []client.Object{
		&eksv1alpha1.PodIdentityAssociation{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PodIdentityAssociationName(role), Namespace: role.Namespace}},
		&iamv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace}},
		&iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace}},
	}

	if err := deleteAllIgnoreMissing(ctx, r.Client, deletes); err != nil {
		return err
	}

	if err := removeFinalizer(ctx, r.Client, role, identityv1.ServiceAccountRoleFinalizer); err != nil {
		return err
	}

	log.Info("finalizer removed", "awio.operation", "remove_finalizer")
	recordFinalizerRemoved(r.Recorder, role)

	return nil
}

func (r *AWSServiceAccountRoleReconciler) cleanupRemoteServiceAccountAnnotations(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole) error {
	if role.Status.DeliveryType != "" {
		return r.cleanupRemoteServiceAccountAnnotationsFromRecordedStatus(ctx, log, role)
	}

	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: identityv1.DefaultName}, config); err != nil {
		if apierrors.IsNotFound(err) {
			return r.cleanupRemoteServiceAccountAnnotationsFromRecordedStatus(ctx, log, role)
		}

		return fmt.Errorf("get AWSWorkloadIdentityConfig/default for remote ServiceAccount annotation cleanup in namespace %q: %w", role.Namespace, err)
	}

	if config.Spec.Type != identityv1.DeliveryTypeSelfHostedIRSA {
		return nil
	}

	roleARN, err := r.roleARNForDeletion(ctx, role)
	if err != nil {
		return err
	}

	if roleARN == "" {
		log.V(1).Info("skipping remote ServiceAccount annotation cleanup because no IAM Role ARN has been observed")

		return nil
	}

	resolved, err := r.Resolver.Resolve(ctx, role.Namespace)
	if err != nil {
		return fmt.Errorf("resolve inventory for remote ServiceAccount annotation cleanup in namespace %q: %w", role.Namespace, err)
	}

	if !resolved.Ready {
		return fmt.Errorf("inventory not ready for remote ServiceAccount annotation cleanup in namespace %q: %s: %s", role.Namespace, resolved.Reason, resolved.Message)
	}

	return r.cleanupRemoteServiceAccountAnnotationsWithResolution(ctx, log, role, &resolved, roleARN)
}

func (r *AWSServiceAccountRoleReconciler) cleanupRemoteServiceAccountAnnotationsFromRecordedStatus(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole) error {
	switch role.Status.DeliveryType {
	case "":
		return fmt.Errorf("AWSWorkloadIdentityConfig/default is gone and AWSServiceAccountRole.status.deliveryType is empty; cannot safely clean remote ServiceAccount annotations")
	case identityv1.DeliveryTypeEKSPodIdentity:
		log.V(1).Info("skipping remote ServiceAccount annotation cleanup based on recorded delivery type", "awio.delivery.type", string(role.Status.DeliveryType))

		return nil
	case identityv1.DeliveryTypeSelfHostedIRSA:
		clusterName, err := namespacedNameFromString(role.Status.ResolvedClusterName)
		if err != nil {
			return fmt.Errorf("AWSServiceAccountRole.status.resolvedClusterName is unusable for remote ServiceAccount annotation cleanup: %w", err)
		}

		roleARN, err := r.roleARNForDeletion(ctx, role)
		if err != nil {
			return err
		}

		if roleARN == "" {
			log.V(1).Info("skipping remote ServiceAccount annotation cleanup because no IAM Role ARN has been observed")

			return nil
		}

		log.V(1).Info("cleaning remote ServiceAccount annotations based on recorded delivery status", "awio.resolvedClusterName", clusterName.String())

		return r.cleanupRemoteServiceAccountAnnotationsWithResolution(ctx, log, role, &inventory.Resolution{
			ClusterName: clusterName,
			Ready:       true,
		}, roleARN)
	default:
		return fmt.Errorf("AWSServiceAccountRole.status.deliveryType %q is unsupported for remote ServiceAccount annotation cleanup", role.Status.DeliveryType)
	}
}

func namespacedNameFromString(value string) (types.NamespacedName, error) {
	namespace, name, ok := strings.Cut(value, "/")
	if !ok || namespace == "" || name == "" {
		return types.NamespacedName{}, fmt.Errorf("expected namespace/name, got %q", value)
	}

	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}

func (r *AWSServiceAccountRoleReconciler) roleARNForDeletion(ctx context.Context, role *identityv1.AWSServiceAccountRole) (string, error) {
	if role.Status.RoleARN != "" {
		return role.Status.RoleARN, nil
	}

	ackRole := &iamv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ackRole), ackRole); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}

		return "", fmt.Errorf("get IAM Role for remote ServiceAccount annotation cleanup in namespace %q: %w", role.Namespace, err)
	}

	return identityaws.ARN(ackRole.Status.ACKResourceMetadata), nil
}

func (r *AWSServiceAccountRoleReconciler) cleanupRemoteServiceAccountAnnotationsWithResolution(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, resolved *inventory.Resolution, roleARN string) error {
	target, err := remoteClusterClient(ctx, r.MCManager, resolved)
	if err != nil {
		return fmt.Errorf("resolve remote cluster client for remote ServiceAccount annotation cleanup: %w", err)
	}

	op, err := removeRemoteServiceAccountAnnotations(ctx, target, role.Spec.ServiceAccount, roleARN)
	logRemoteApply(log, metrics.ResourceServiceAccount, serviceAccountIndexKey(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name), op, err)

	if err != nil {
		return fmt.Errorf("remove remote ServiceAccount annotations %s/%s: %w", role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name, err)
	}

	return nil
}

func (r *AWSServiceAccountRoleReconciler) patchRoleStatus(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus) error {
	if apiequality.Semantic.DeepEqual(*beforeStatus, role.Status) {
		return nil
	}

	patchBase := role.DeepCopy()
	patchBase.Status = *beforeStatus

	return patchStatusAndObserve(ctx, log, r.Status(), r.Recorder, metrics.ControllerRole, role, patchBase, beforeStatus.Conditions, role.Status.Conditions)
}

// SetupWithManager registers the reconciler with a controller manager.
func (r *AWSServiceAccountRoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Resolver.Client == nil {
		r.Resolver = inventory.Resolver{Client: r.Client}
	}

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&identityv1.AWSServiceAccountRole{}).
		Owns(&iamv1alpha1.Policy{}).
		Owns(&iamv1alpha1.Role{}).
		Watches(&identityv1.AWSWorkloadIdentityConfig{}, handler.EnqueueRequestsFromMapFunc(r.rolesForConfig)).
		Watches(&identityv1.AWSWorkloadIdentityOperatorConfig{}, handler.EnqueueRequestsFromMapFunc(r.rolesForOperatorConfig)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles})

	if hasPodIdentityAssociationCRD(mgr) {
		builder = builder.Owns(&eksv1alpha1.PodIdentityAssociation{})
	}

	if r.RoleEnqueueChannel != nil {
		builder = builder.WatchesRawSource(source.Channel[*identityv1.AWSServiceAccountRole](
			r.RoleEnqueueChannel,
			&handler.TypedEnqueueRequestForObject[*identityv1.AWSServiceAccountRole]{},
		))
	}

	if err := builder.Complete(r); err != nil {
		return fmt.Errorf("set up AWSServiceAccountRole controller: %w", err)
	}

	return nil
}

func (r *AWSServiceAccountRoleReconciler) rolesForConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	if !isDefaultObject(obj) {
		return nil
	}

	log := watchMapLogger(ctx, metrics.ControllerRole, "rolesForConfig", "AWSWorkloadIdentityConfig", obj)

	return requestsForList(ctx, log, r.Client, &identityv1.AWSServiceAccountRoleList{}, client.InNamespace(obj.GetNamespace()))
}

func (r *AWSServiceAccountRoleReconciler) rolesForOperatorConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	if !isDefaultObject(obj) {
		return nil
	}

	log := watchMapLogger(ctx, metrics.ControllerRole, "rolesForOperatorConfig", "AWSWorkloadIdentityOperatorConfig", obj)

	return requestsForList(ctx, log, r.Client, &identityv1.AWSServiceAccountRoleList{})
}

func hasPodIdentityAssociationCRD(mgr ctrl.Manager) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(eksv1alpha1.GroupVersion.WithKind(ackChildKindPodIdentityAssociation).GroupKind(), eksv1alpha1.GroupVersion.Version)

	return err == nil
}
