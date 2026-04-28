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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	crbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
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
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles/finalizers,verbs=update
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityoperatorconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles,verbs=list;watch
// +kubebuilder:rbac:groups=iam.services.k8s.aws,resources=roles;policies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eks.services.k8s.aws,resources=podidentityassociations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
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
	log = log.WithValues(logKeyDeliveryType, string(delivery))
	ctx = logf.IntoContext(ctx, log)

	setRoleResolvedDeliveryStatus(role, delivery, &inputs.resolved)
	pruneStaleRoleACKResources(role, delivery)

	policyARNs, err := r.reconcileGeneratedPolicy(ctx, log, role, delivery, role.Spec.PolicyARNs)
	if err != nil {
		return r.returnWithRoleStatusPatch(ctx, log, role, beforeStatus, err)
	}

	if err := r.reconcileIAMRole(ctx, log, role, inputs, policyARNs); err != nil {
		return r.returnWithRoleStatusPatch(ctx, log, role, beforeStatus, err)
	}

	if shouldPersistAnnotationDeliveryContextBeforeRemotePatch(role, beforeStatus, delivery) {
		message := "waiting to retry remote ServiceAccount annotation delivery after persisting role status"
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, message)
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, identityv1.ReasonRemoteDeliveryPending, message)

		if err := r.patchRoleStatus(ctx, log, role, beforeStatus); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	result, err = r.setDeliveryConditions(ctx, log, role, delivery, inputs, beforeStatus)
	if err != nil {
		return r.returnWithRoleStatusPatch(ctx, log, role, beforeStatus, err)
	}

	setHubReadyConditions(role, delivery, inputs.config)

	if err := r.patchRoleStatus(ctx, log, role, beforeStatus); err != nil {
		return ctrl.Result{}, err
	}

	return roleResultWithAnnotationDeliverySafetyRequeue(delivery, role, result), nil
}

func shouldPersistAnnotationDeliveryContextBeforeRemotePatch(role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus, delivery identityv1.DeliveryType) bool {
	return delivery.UsesAnnotationBasedIRSA() &&
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
	failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, identityv1.ReasonInvalidSpec, message)

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
		log.V(1).Info("operator configuration unavailable", logKeyOperation, logOpLoadOperatorCfg)

		return r.failPrepareStep(ctx, log, role, beforeStatus, identityv1.ConditionOperatorConfigReady, identityv1.ReasonOperatorConfigUnavailable, err.Error())
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionOperatorConfigReady, metav1.ConditionTrue, identityv1.ReasonReady, "operator configuration is valid")

	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: identityv1.DefaultName}, config); err != nil {
		return r.failPrepareStep(ctx, log, role, beforeStatus, identityv1.ConditionConfigResolved, identityv1.ReasonConfigUnavailable, err.Error())
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionConfigResolved, metav1.ConditionTrue, identityv1.ReasonResolved, "namespace config resolved")

	resolved, result, err := r.resolveInventory(ctx, log, role, beforeStatus)
	if err != nil {
		return nil, result, err
	}

	trustPolicy, err := r.trustPolicy(role, config, &resolved)
	if err != nil {
		return r.failPrepareStep(ctx, log, role, beforeStatus, identityv1.ConditionTrustPolicyReady, identityv1.ReasonTrustPolicyInputMissing, err.Error())
	}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionTrustPolicyReady, metav1.ConditionTrue, identityv1.ReasonRendered, "trust policy rendered")

	return &roleReconcileInputs{
		operatorConfig: operatorConfig,
		config:         config,
		resolved:       resolved,
		trustPolicy:    trustPolicy,
	}, ctrl.Result{}, nil
}

func (r *AWSServiceAccountRoleReconciler) failPrepareStep(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus, condType, reason, message string) (*roleReconcileInputs, ctrl.Result, error) {
	failReady(&role.Status.Conditions, role.Generation, condType, reason, message)

	if patchErr := r.patchRoleStatus(ctx, log, role, beforeStatus); patchErr != nil {
		return nil, ctrl.Result{}, patchErr
	}

	return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
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
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, resolved.Reason, resolved.Message)
		log.V(1).Info("waiting for inventory resolution", logKeyConditionReason, resolved.Reason)

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

	if delivery.UsesAnnotationBasedIRSA() && resolved != nil && resolved.Ready {
		role.Status.ResolvedClusterName = resolved.ClusterName.String()
	}
}

// upsertRoleACKResource replaces an existing ACKResource entry with the same
// (apiVersion, kind, namespace, name) tuple, or appends a new one. The match
// key mirrors the listMapKey declared on
// AWSServiceAccountRoleStatus.ACKResources in awsserviceaccountrole_types.go,
// keeping in-memory equivalence aligned with the CRD's associative list
// semantics. Idempotent per-key entries preserve previously observed ACK
// metadata when a downstream step errors mid-rebuild.
func upsertRoleACKResource(role *identityv1.AWSServiceAccountRole, entry *identityv1.ACKResourceStatus) {
	for i, existing := range role.Status.ACKResources {
		if existing.APIVersion == entry.APIVersion && existing.Kind == entry.Kind && existing.Namespace == entry.Namespace && existing.Name == entry.Name {
			role.Status.ACKResources[i] = *entry

			return
		}
	}

	role.Status.ACKResources = append(role.Status.ACKResources, *entry)
}

// removeRoleACKResourceByKind drops every entry whose Kind equals kind. Used
// when a controller-managed ACK CR is intentionally deleted (e.g. generated
// IAM Policy removed when spec.PolicyDocument is cleared) or no longer applies
// to the current delivery type.
func removeRoleACKResourceByKind(role *identityv1.AWSServiceAccountRole, kind string) {
	role.Status.ACKResources = slices.DeleteFunc(role.Status.ACKResources, func(s identityv1.ACKResourceStatus) bool {
		return s.Kind == kind
	})
}

// pruneStaleRoleACKResources removes ACK resource entries that are no longer
// relevant for the current delivery type. The reconcile* helpers upsert their
// own entries idempotently; this clears stale ones left from a previous
// delivery configuration so a transition (e.g. EKSPodIdentity ->
// SelfHostedIRSA) does not leak an orphaned PodIdentityAssociation status
// entry. IAM Role and IAM Policy entries are reused across both delivery
// types, so they are not pruned here; reconcileGeneratedPolicy handles
// Policy lifecycle when spec.PolicyDocument is cleared.
func pruneStaleRoleACKResources(role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType) {
	if delivery != identityv1.DeliveryTypeEKSPodIdentity {
		removeRoleACKResourceByKind(role, ackChildKindPodIdentityAssociation)
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
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionRoleReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, err.Error())
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, identityv1.ReasonChildApplyFailed, err.Error())

		return err
	}

	roleEntry := identityaws.ACKResourceStatus(iamv1alpha1.GroupVersion.String(), ackChildKindRole, current, current.Status.Conditions)
	upsertRoleACKResource(role, &roleEntry)
	role.Status.RoleARN = identityaws.ARN(current.Status.ACKResourceMetadata)
	setACKReadyCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionRoleReady, "IAM Role", identityaws.IsACKSynced(current.Status.Conditions), role.Status.RoleARN != "")

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
	case identityv1.DeliveryTypeSelfHostedIRSA, identityv1.DeliveryTypeEKSIRSA:
		if !meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionServiceAccountAnnotationReady) {
			return metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, "waiting for remote ServiceAccount annotations"
		}

		if !meta.IsStatusConditionTrue(config.Status.Conditions, identityv1.ConditionReady) {
			return metav1.ConditionFalse, identityv1.ReasonConfigNotReady, fmt.Sprintf("waiting for AWSWorkloadIdentityConfig/%s/%s to become Ready", config.Namespace, config.Name)
		}
	case identityv1.DeliveryTypeEKSPodIdentity:
		if !meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionPodIdentityAssocReady) {
			return metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for ACK PodIdentityAssociation"
		}

		agent := meta.FindStatusCondition(role.Status.Conditions, identityv1.ConditionPodIdentityAgentReady)
		if agent == nil || agent.Status == metav1.ConditionUnknown {
			return metav1.ConditionFalse, identityv1.ReasonRemoteCheckPending, "waiting for remote EKS Pod Identity Agent readiness check"
		}

		if agent.Status != metav1.ConditionTrue {
			return metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, "remote EKS Pod Identity Agent is not ready"
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

	saPatchOp, err := r.reconcileAnnotationBasedIRSADelivery(ctx, log, role, delivery, inputs)
	if err != nil {
		// Log explicitly: returning nil below suppresses controller-runtime's own error log.
		log.Error(err, "annotation-based IRSA delivery deferred", logKeyConditionReason, identityv1.ReasonRemoteDeliveryPending)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending, err.Error())

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	annotationReason := serviceAccountAnnotationSyncReason(r.Recorder, role, beforeStatus, saPatchOp)

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, annotationReason, "remote ServiceAccount annotations are synced")

	return ctrl.Result{}, nil
}

func roleResultWithAnnotationDeliverySafetyRequeue(delivery identityv1.DeliveryType, role *identityv1.AWSServiceAccountRole, result ctrl.Result) ctrl.Result {
	if delivery.UsesAnnotationBasedIRSA() &&
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

func (r *AWSServiceAccountRoleReconciler) reconcileAnnotationBasedIRSADelivery(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, inputs *roleReconcileInputs) (controllerutil.OperationResult, error) {
	target, err := remoteClusterClient(ctx, r.MCManager, &inputs.resolved)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	op, err := patchRemoteServiceAccountAnnotations(ctx, target, role.Spec.ServiceAccount, role.Status.RoleARN)
	logRemoteApplyForDelivery(log, delivery, metrics.ResourceServiceAccount, serviceAccountIndexKey(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name), op, err)

	return op, err
}

func (r *AWSServiceAccountRoleReconciler) reconcileGeneratedPolicy(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, policyARNs []string) ([]string, error) {
	if role.Spec.PolicyDocument == "" {
		policy := &iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace}}
		// Verify controllerRef UID before deleting to avoid ABA
		// collateral-deletion of a foreign object sharing the same generated
		// name (e.g. a Policy authored by a recreated-with-same-name role or
		// by another controller).
		if _, err := r.deleteRoleChildIfOwned(ctx, log, role, policy); err != nil {
			return nil, fmt.Errorf("prune generated IAM Policy %s/%s: %w", policy.Namespace, policy.Name, err)
		}

		role.Status.GeneratedPolicyARN = ""
		removeRoleACKResourceByKind(role, ackChildKindPolicy)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, metav1.ConditionTrue, identityv1.ReasonManagedPoliciesOnly, "managed policy ARNs are allowed")

		return policyARNs, nil
	}

	policyDoc, err := identityaws.PolicyDocumentString(role.Spec.PolicyDocument)
	if err != nil {
		wrapped := fmt.Errorf("canonicalize policy document: %w", err)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec, wrapped.Error())
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, identityv1.ReasonInvalidSpec, wrapped.Error())

		return nil, wrapped
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
		wrapped := fmt.Errorf("apply IAM Policy %s/%s: %w", current.Namespace, current.Name, err)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, wrapped.Error())
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, identityv1.ReasonChildApplyFailed, wrapped.Error())

		return nil, wrapped
	}

	policyEntry := identityaws.ACKResourceStatus(iamv1alpha1.GroupVersion.String(), ackChildKindPolicy, current, current.Status.Conditions)
	upsertRoleACKResource(role, &policyEntry)
	role.Status.GeneratedPolicyARN = identityaws.ARN(current.Status.ACKResourceMetadata)

	if role.Status.GeneratedPolicyARN != "" {
		// Clone before append so we never mutate the caller's spec-backed slice
		// when the underlying array still has capacity.
		policyARNs = append(slices.Clone(policyARNs), role.Status.GeneratedPolicyARN)
	}

	setACKReadyCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, "generated IAM Policy", identityaws.IsACKSynced(current.Status.Conditions), role.Status.GeneratedPolicyARN != "")

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
		wrapped := fmt.Errorf("apply PodIdentityAssociation %s/%s: %w", current.Namespace, current.Name, err)
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAssocReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, wrapped.Error())
		failReady(&role.Status.Conditions, role.Generation, identityv1.ConditionDeliveryReady, identityv1.ReasonChildApplyFailed, wrapped.Error())

		return wrapped
	}

	piaEntry := identityaws.ACKResourceStatus(eksv1alpha1.GroupVersion.String(), ackChildKindPodIdentityAssociation, current, current.Status.Conditions)
	upsertRoleACKResource(role, &piaEntry)

	setACKReadyCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAssocReady, "EKS PodIdentityAssociation", identityaws.IsACKSynced(current.Status.Conditions), true)

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
	case identityv1.DeliveryTypeSelfHostedIRSA, identityv1.DeliveryTypeEKSIRSA:
		if config.Status.OIDCProviderARN == "" {
			return "", fmt.Errorf("AWSWorkloadIdentityConfig.status.oidcProviderARN is empty")
		}

		if config.Status.IssuerHostPath == "" {
			return "", fmt.Errorf("AWSWorkloadIdentityConfig.status.issuerHostPath is empty")
		}

		policy, err := identityaws.WebIdentityTrustPolicy(config.Status.IssuerHostPath, config.Status.OIDCProviderARN, role.Spec.ServiceAccount)
		if err != nil {
			return "", fmt.Errorf("build web identity trust policy: %w", err)
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
		if patchErr := r.markRoleDeletionBlocked(ctx, log, role, identityv1.ReasonRemoteClusterUnavailable, err.Error()); patchErr != nil {
			log.Error(patchErr, "failed to patch DeletionBlocked status during remote annotation cleanup")
		}

		return err
	}

	// PodIdentityAssociation must be deleted before the IAM Role/Policy it
	// references; each child is ownership-verified by controllerRef UID to
	// avoid ABA collateral-deletion of a foreign object sharing the same
	// generated name.
	children := []client.Object{
		&eksv1alpha1.PodIdentityAssociation{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PodIdentityAssociationName(role), Namespace: role.Namespace}},
		&iamv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace}},
		&iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace}},
	}

	var pending []string

	for _, child := range children {
		stillPresent, err := r.deleteRoleChildIfOwned(ctx, log, role, child)
		if err != nil {
			if patchErr := r.markRoleDeletionBlocked(ctx, log, role, identityv1.ReasonChildApplyFailed, err.Error()); patchErr != nil {
				log.Error(patchErr, "failed to patch DeletionBlocked status during child delete")
			}

			return err
		}

		if stillPresent {
			pending = append(pending, fmt.Sprintf("%T %s/%s", child, child.GetNamespace(), child.GetName()))
		}
	}

	if len(pending) > 0 {
		message := fmt.Sprintf("waiting for ACK child resources to finish deletion: %s", strings.Join(pending, ", "))
		if patchErr := r.markRoleDeletionBlocked(ctx, log, role, identityv1.ReasonChildrenPending, message); patchErr != nil {
			log.Error(patchErr, "failed to patch DeletionBlocked status while waiting for ACK child deletion")
		}

		return errors.New(message)
	}

	if err := r.clearRoleDeletionBlocked(ctx, log, role); err != nil {
		return err
	}

	return removeFinalizer(ctx, r.Client, r.Recorder, log, role, identityv1.ServiceAccountRoleFinalizer)
}

// deleteRoleChildIfOwned deletes a generated ACK child only after confirming
// its controllerRef UID matches role. Verifying the controllerRef stamp left
// by createOrUpdate prevents ABA collateral-deletion when a foreign controller
// or a concurrently-recreated object happens to share the same generated name.
//
// Returns stillPresent=true when, after issuing the Delete, the API server
// continues to observe the owned child — typically because ACK has not yet
// removed its finalizer while AWS-side teardown is in flight. Callers on the
// finalization path must hold the parent finalizer in that case, since the
// parent must not disappear before its owned ACK children are gone from the
// API server (Deletion Guardrails, AGENTS.md). stillPresent=false means the
// child is already gone (NotFound / NoKindMatch), is not owned by this role
// (foreign object left intact), or finished deletion synchronously because it
// carried no finalizer.
func (r *AWSServiceAccountRoleReconciler) deleteRoleChildIfOwned(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, child client.Object) (bool, error) {
	key := client.ObjectKeyFromObject(child)
	if err := r.Get(ctx, key, child); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}

		return false, fmt.Errorf("get %T %s/%s: %w", child, key.Namespace, key.Name, err)
	}

	if !isRoleChildOwnedBy(role, child) {
		log.Info("skipping delete: child is not controlled by this AWSServiceAccountRole",
			"awio.child.kind", fmt.Sprintf("%T", child),
			"awio.child.namespace", key.Namespace,
			"awio.child.name", key.Name)

		return false, nil
	}

	if err := r.Delete(ctx, child); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("delete %T %s/%s: %w", child, key.Namespace, key.Name, err)
	}

	// Re-check whether the API server still observes the child. A finalizer-
	// bearing ACK child remains visible (with DeletionTimestamp set) until ACK
	// completes AWS-side teardown and removes its own finalizer; a child with
	// no finalizer is gone synchronously after Delete returns.
	if err := r.Get(ctx, key, child); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}

		return false, fmt.Errorf("re-check %T %s/%s after delete: %w", child, key.Namespace, key.Name, err)
	}

	return true, nil
}

// isRoleChildOwnedBy reports whether obj's controller-style OwnerReference
// belongs to role. createOrUpdate stamps generated ACK CRs with a controller
// reference, so finalization must verify that stamp before deleting a child by
// name — a missing or mismatched controllerRef means the object was authored
// elsewhere and must be left intact.
func isRoleChildOwnedBy(role *identityv1.AWSServiceAccountRole, obj client.Object) bool {
	if role == nil || obj == nil || role.UID == "" {
		return false
	}

	if owner := metav1.GetControllerOf(obj); owner != nil && owner.UID == role.UID {
		return true
	}

	return false
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

	if !config.Spec.Type.UsesAnnotationBasedIRSA() {
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

	return r.cleanupRemoteServiceAccountAnnotationsWithResolution(ctx, log, role, config.Spec.Type, &resolved, roleARN)
}

func (r *AWSServiceAccountRoleReconciler) cleanupRemoteServiceAccountAnnotationsFromRecordedStatus(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole) error {
	switch role.Status.DeliveryType {
	case "":
		return fmt.Errorf("AWSWorkloadIdentityConfig/default is gone and AWSServiceAccountRole.status.deliveryType is empty; cannot safely clean remote ServiceAccount annotations")
	case identityv1.DeliveryTypeEKSPodIdentity:
		log.V(1).Info("skipping remote ServiceAccount annotation cleanup based on recorded delivery type", logKeyDeliveryType, string(role.Status.DeliveryType))

		return nil
	case identityv1.DeliveryTypeSelfHostedIRSA, identityv1.DeliveryTypeEKSIRSA:
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

		return r.cleanupRemoteServiceAccountAnnotationsWithResolution(ctx, log, role, role.Status.DeliveryType, &inventory.Resolution{
			ClusterName: clusterName,
			Ready:       true,
		}, roleARN)
	default:
		return fmt.Errorf("AWSServiceAccountRole.status.deliveryType %q is unsupported for remote ServiceAccount annotation cleanup", role.Status.DeliveryType)
	}
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

func (r *AWSServiceAccountRoleReconciler) cleanupRemoteServiceAccountAnnotationsWithResolution(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType, resolved *inventory.Resolution, roleARN string) error {
	target, err := remoteClusterClient(ctx, r.MCManager, resolved)
	if err != nil {
		return fmt.Errorf("resolve remote cluster client for remote ServiceAccount annotation cleanup: %w", err)
	}

	op, err := removeRemoteServiceAccountAnnotations(ctx, target, role.Spec.ServiceAccount, roleARN)
	logRemoteApplyForDelivery(log, delivery, metrics.ResourceServiceAccount, serviceAccountIndexKey(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name), op, err)

	if err != nil {
		return fmt.Errorf("remove remote ServiceAccount annotations %s/%s: %w", role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name, err)
	}

	return nil
}

// markRoleDeletionBlocked surfaces a DeletionBlocked condition on the role so
// operators can observe why finalization is paused. Mirrors
// AWSWorkloadIdentityConfigReconciler.markDeletionBlocked.
func (r *AWSServiceAccountRoleReconciler) markRoleDeletionBlocked(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, reason, message string) error {
	beforeStatus := role.Status.DeepCopy()
	role.Status.ObservedGeneration = role.Generation
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionDeletionBlocked, metav1.ConditionTrue, reason, message)
	log.Info("deletion blocked", logKeyConditionType, identityv1.ConditionDeletionBlocked, logKeyConditionReason, reason)

	return r.patchRoleStatus(ctx, log, role, beforeStatus)
}

// clearRoleDeletionBlocked emits False only when transitioning from True so
// observers see the unblock event; otherwise no-op.
func (r *AWSServiceAccountRoleReconciler) clearRoleDeletionBlocked(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole) error {
	if !meta.IsStatusConditionTrue(role.Status.Conditions, identityv1.ConditionDeletionBlocked) {
		return nil
	}

	beforeStatus := role.Status.DeepCopy()
	role.Status.ObservedGeneration = role.Generation
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionDeletionBlocked, metav1.ConditionFalse, identityv1.ReasonDeletionUnblocked, "deletion is no longer blocked")
	log.Info("deletion unblocked", logKeyConditionType, identityv1.ConditionDeletionBlocked, logKeyConditionReason, identityv1.ReasonDeletionUnblocked)

	return r.patchRoleStatus(ctx, log, role, beforeStatus)
}

func (r *AWSServiceAccountRoleReconciler) patchRoleStatus(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus) error {
	if apiequality.Semantic.DeepEqual(*beforeStatus, role.Status) {
		return nil
	}

	patchBase := role.DeepCopy()
	patchBase.Status = *beforeStatus

	return patchStatusAndObserve(ctx, log, r.Status(), r.Recorder, metrics.ControllerRole, role, patchBase, beforeStatus.Conditions, role.Status.Conditions)
}

// returnWithRoleStatusPatch persists status before returning err so condition
// transitions recorded earlier survive a transient helper error.
func (r *AWSServiceAccountRoleReconciler) returnWithRoleStatusPatch(ctx context.Context, log logr.Logger, role *identityv1.AWSServiceAccountRole, beforeStatus *identityv1.AWSServiceAccountRoleStatus, err error) (ctrl.Result, error) {
	if patchErr := r.patchRoleStatus(ctx, log, role, beforeStatus); patchErr != nil {
		log.Error(patchErr, "failed to patch role status before returning helper error")
	}

	return ctrl.Result{}, err
}

// SetupWithManager registers the reconciler with a controller manager.
func (r *AWSServiceAccountRoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Resolver.Client == nil {
		r.Resolver = inventory.Resolver{Client: r.Client}
	}

	rootChanged := crbuilder.WithPredicates(rootObjectChangedPredicate(metrics.ControllerRole))

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&identityv1.AWSServiceAccountRole{}, rootChanged).
		Owns(&iamv1alpha1.Policy{}).
		Owns(&iamv1alpha1.Role{}).
		Watches(&identityv1.AWSWorkloadIdentityConfig{}, handler.EnqueueRequestsFromMapFunc(r.rolesForConfig)).
		Watches(&identityv1.AWSWorkloadIdentityOperatorConfig{},
			handler.EnqueueRequestsFromMapFunc(r.rolesForOperatorConfig),
			rootChanged).
		Watches(&clusterinventoryv1alpha1.ClusterProfile{},
			handler.EnqueueRequestsFromMapFunc(r.rolesForClusterProfile)).
		Watches(&identityv1.AWSServiceAccountRole{},
			handler.EnqueueRequestsFromMapFunc(r.rolesForSiblingServiceAccountBinding),
			crbuilder.WithPredicates(siblingServiceAccountBindingChangedPredicate(metrics.ControllerRole))).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles})

	piaGVK := eksv1alpha1.GroupVersion.WithKind(ackChildKindPodIdentityAssociation)

	piaPresent, err := hasMapping(mgr.GetRESTMapper(), piaGVK)
	if err != nil {
		return fmt.Errorf("probe optional CRD %s: %w", piaGVK, err)
	}

	if piaPresent {
		builder = builder.Owns(&eksv1alpha1.PodIdentityAssociation{})
	} else {
		builder = builder.Watches(
			&apiextensionsv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(r.rolesForAckChildCRD),
			crbuilder.WithPredicates(ackChildCRDChangedPredicate(metrics.ControllerRole, ackChildCRDNamePodIdentityAssociation)),
		)
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

	return requestsForList(ctx, log, r.Client, metrics.ControllerRole, "rolesForConfig", "AWSWorkloadIdentityConfig", &identityv1.AWSServiceAccountRoleList{}, client.InNamespace(obj.GetNamespace()))
}

func (r *AWSServiceAccountRoleReconciler) rolesForOperatorConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	if !isDefaultObject(obj) {
		return nil
	}

	log := watchMapLogger(ctx, metrics.ControllerRole, "rolesForOperatorConfig", "AWSWorkloadIdentityOperatorConfig", obj)

	return requestsForList(ctx, log, r.Client, metrics.ControllerRole, "rolesForOperatorConfig", "AWSWorkloadIdentityOperatorConfig", &identityv1.AWSServiceAccountRoleList{})
}

func (r *AWSServiceAccountRoleReconciler) rolesForClusterProfile(ctx context.Context, obj client.Object) []reconcile.Request {
	profile, ok := obj.(*clusterinventoryv1alpha1.ClusterProfile)
	if !ok {
		return nil
	}

	namespace := inventory.WorkloadNamespaceForClusterProfile(profile)
	if namespace == "" {
		return nil
	}

	log := watchMapLogger(ctx, metrics.ControllerRole, "rolesForClusterProfile", "ClusterProfile", obj)

	return requestsForList(ctx, log, r.Client, metrics.ControllerRole, "rolesForClusterProfile", "ClusterProfile", &identityv1.AWSServiceAccountRoleList{}, client.InNamespace(namespace))
}

func (r *AWSServiceAccountRoleReconciler) rolesForAckChildCRD(ctx context.Context, obj client.Object) []reconcile.Request {
	log := watchMapLogger(ctx, metrics.ControllerRole, "rolesForAckChildCRD", "CustomResourceDefinition", obj)

	return requestsForList(ctx, log, r.Client, metrics.ControllerRole, "rolesForAckChildCRD", "CustomResourceDefinition", &identityv1.AWSServiceAccountRoleList{})
}

// rolesForSiblingServiceAccountBinding enqueues every other AWSServiceAccountRole
// in the trigger's namespace that currently binds the same ServiceAccount, so
// duplicate-binding conflicts surfaced by reconcileDuplicateBindingConflict
// clear event-driven instead of waiting for the 30s transient RequeueAfter.
//
// spec.serviceAccount is immutable (CEL marker on AWSServiceAccountRoleSpec),
// so a sibling can only move in or out of a conflict set via create,
// deletion-timestamp transition, or full delete — gated by
// siblingServiceAccountBindingChangedPredicate. The trigger role itself is
// excluded because For() drives its own enqueue path.
func (r *AWSServiceAccountRoleReconciler) rolesForSiblingServiceAccountBinding(ctx context.Context, obj client.Object) []reconcile.Request {
	role, ok := obj.(*identityv1.AWSServiceAccountRole)
	if !ok {
		return nil
	}

	if role.Spec.ServiceAccount.Namespace == "" || role.Spec.ServiceAccount.Name == "" {
		return nil
	}

	log := watchMapLogger(ctx, metrics.ControllerRole, "rolesForSiblingServiceAccountBinding", "AWSServiceAccountRole", obj)

	requests := requestsForList(ctx, log, r.Client, metrics.ControllerRole, "rolesForSiblingServiceAccountBinding", "AWSServiceAccountRole", &identityv1.AWSServiceAccountRoleList{},
		client.InNamespace(role.Namespace),
		roleByServiceAccountKey(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name),
	)

	self := client.ObjectKeyFromObject(role)
	siblings := requests[:0]

	for _, req := range requests {
		if req.NamespacedName == self {
			continue
		}

		siblings = append(siblings, req)
	}

	return siblings
}
