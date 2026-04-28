package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

// SelfHostedServiceAccountReconciler keeps remote workload ServiceAccount
// annotations in sync for annotation-based IRSA.
type SelfHostedServiceAccountReconciler struct {
	LocalClient             client.Client
	MCManager               mcmanager.Manager
	Resolver                inventory.Resolver
	Recorder                events.EventRecorder
	MaxConcurrentReconciles int
}

// Reconcile repairs aws-pod-identity-webhook annotations when a remote
// workload ServiceAccount changes.
func (r *SelfHostedServiceAccountReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (result ctrl.Result, reconcileErr error) {
	namespace := inventoryNamespaceFromCluster(req.ClusterName.String())
	log := loggerForRemoteRequest(ctx, metrics.ControllerSelfHostedServiceAccount, req, namespace)
	ctx = logf.IntoContext(ctx, log)
	log.V(1).Info("starting reconcile")

	defer func() {
		logReconcileEnd(log, result, reconcileErr)
	}()

	if namespace == "" {
		metrics.RecordRemoteDelivery("", metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonNoNamespace)

		return ctrl.Result{}, nil
	}

	target, result, err := resolveAnnotationBasedIRSATarget(ctx, &selfHostedTargetRequest{
		LocalClient: r.LocalClient,
		Resolver:    r.Resolver,
		MCManager:   r.MCManager,
		ClusterName: req.ClusterName,
		Namespace:   namespace,
		Resource:    metrics.ResourceServiceAccount,
	}, log)
	if errors.Is(err, errReconcileDone) {
		return result, nil
	}

	if err != nil {
		return result, err
	}

	return ctrl.Result{}, r.reconcileRemoteServiceAccount(ctx, log, target.Cluster.GetClient(), target.DeliveryType, namespace, req.NamespacedName)
}

func (r *SelfHostedServiceAccountReconciler) reconcileRemoteServiceAccount(ctx context.Context, log logr.Logger, remoteClient client.Client, deliveryType identityv1.DeliveryType, inventoryNamespace string, key types.NamespacedName) error {
	current := &corev1.ServiceAccount{}
	if err := remoteClient.Get(ctx, key, current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("get remote ServiceAccount %s: %w", key, err)
	}

	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := r.LocalClient.List(ctx, roles, client.InNamespace(inventoryNamespace), roleByServiceAccountKey(key.Namespace, key.Name)); err != nil {
		return fmt.Errorf("list AWSServiceAccountRoles by ServiceAccount index in namespace %q: %w", inventoryNamespace, err)
	}

	activeRoles := activeRolesForServiceAccount(roles.Items)

	// Multiple active AWSServiceAccountRole objects bind the same ServiceAccount:
	// indexer order is non-deterministic, so picking any one Role's RoleARN would
	// be unstable across reconciles. The conflict is already surfaced on the
	// owning Role via ConditionDeliveryReady=False / ReasonInvalidSpec by the
	// local Role controller; the next reconcile after the conflict is resolved
	// delivers the correct annotation.
	if len(activeRoles) > 1 {
		names := conflictingRoleNames(activeRoles)
		log.Info(logMsgSkipSARepairMultiRole, logKeyConflictingRoles, names)

		return nil
	}

	if len(activeRoles) == 0 || activeRoles[0].Status.RoleARN == "" {
		return nil
	}

	role := activeRoles[0]

	op, err := patchRemoteServiceAccountObjectAnnotations(ctx, remoteClient, current, role.Status.RoleARN)
	logRemoteApplyForDelivery(log, deliveryType, metrics.ResourceServiceAccount, key.String(), op, err)

	if err != nil {
		return fmt.Errorf("patch remote ServiceAccount annotations %s: %w", key, err)
	}

	if op == controllerutil.OperationResultUpdated {
		recordAnnotationRepaired(r.Recorder, role)
	}

	return nil
}

// activeRolesForServiceAccount returns the AWSServiceAccountRole entries with a
// zero deletionTimestamp. This mirrors the active-Role semantics in
// AWSServiceAccountRoleReconciler.conflictingServiceAccountBindingNames so the
// two controllers agree on what counts as a binding conflict.
func activeRolesForServiceAccount(items []identityv1.AWSServiceAccountRole) []*identityv1.AWSServiceAccountRole {
	active := make([]*identityv1.AWSServiceAccountRole, 0, len(items))

	for i := range items {
		role := &items[i]
		if !role.DeletionTimestamp.IsZero() {
			continue
		}

		active = append(active, role)
	}

	return active
}

// conflictingRoleNames returns the lexicographically sorted "namespace/name"
// keys of the supplied AWSServiceAccountRole objects so the Warning event note
// is deterministic regardless of indexer iteration order.
func conflictingRoleNames(roles []*identityv1.AWSServiceAccountRole) []string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, client.ObjectKeyFromObject(role).String())
	}

	slices.Sort(names)

	return names
}

// SetupWithManager registers the remote ServiceAccount annotation repair
// controller.
func (r *SelfHostedServiceAccountReconciler) SetupWithManager(mcMgr mcmanager.Manager) error {
	if err := mcbuilder.ControllerManagedBy(mcMgr).
		Named(metrics.ControllerSelfHostedServiceAccount).
		For(&corev1.ServiceAccount{}, mcbuilder.WithPredicates(annotationRepairPredicate())).
		WithOptions(mccontroller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r); err != nil {
		return fmt.Errorf("set up self-hosted ServiceAccount controller: %w", err)
	}

	return nil
}

func annotationRepairPredicate() predicate.Predicate {
	return metricsRecordingPredicate(metrics.ControllerSelfHostedServiceAccount, predicateKeepFns{
		Create: func(e event.CreateEvent) bool {
			return hasSelfHostedRoleARNAnnotation(e.Object)
		},
		Update: func(e event.UpdateEvent) bool {
			return hasSelfHostedRoleARNAnnotation(e.ObjectOld) || hasSelfHostedRoleARNAnnotation(e.ObjectNew)
		},
		Generic: func(e event.GenericEvent) bool {
			return hasSelfHostedRoleARNAnnotation(e.Object)
		},
	})
}

func hasSelfHostedRoleARNAnnotation(obj client.Object) bool {
	if obj == nil {
		return false
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}

	_, ok := annotations[selfHostedRoleARNAnnotation]

	return ok
}

func patchRemoteServiceAccountAnnotations(ctx context.Context, c client.Client, subject identityv1.ServiceAccountSubject, roleARN string) (controllerutil.OperationResult, error) {
	if roleARN == "" {
		return controllerutil.OperationResultNone, nil
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: subject.Name, Namespace: subject.Namespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sa), sa); err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("get remote ServiceAccount %s/%s: %w", subject.Namespace, subject.Name, err)
	}

	return patchRemoteServiceAccountObjectAnnotations(ctx, c, sa, roleARN)
}

func patchRemoteServiceAccountObjectAnnotations(ctx context.Context, c client.Client, sa *corev1.ServiceAccount, roleARN string) (controllerutil.OperationResult, error) {
	desired := renderSelfHostedServiceAccountAnnotations(roleARN)
	if len(desired) == 0 {
		return controllerutil.OperationResultNone, nil
	}

	current := sa.GetAnnotations()
	changed := false

	for key, value := range desired {
		if current[key] != value {
			changed = true

			break
		}
	}

	if !changed {
		return controllerutil.OperationResultNone, nil
	}

	base := sa.DeepCopy()
	annotations := ensureAnnotations(sa)
	maps.Copy(annotations, desired)

	if err := c.Patch(ctx, sa, client.MergeFrom(base)); err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("patch remote ServiceAccount %s/%s: %w", sa.Namespace, sa.Name, err)
	}

	return controllerutil.OperationResultUpdated, nil
}

func removeRemoteServiceAccountAnnotations(ctx context.Context, c client.Client, subject identityv1.ServiceAccountSubject, roleARN string) (controllerutil.OperationResult, error) {
	if roleARN == "" {
		return controllerutil.OperationResultNone, nil
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: subject.Name, Namespace: subject.Namespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sa), sa); err != nil {
		if apierrors.IsNotFound(err) {
			return controllerutil.OperationResultNone, nil
		}

		return controllerutil.OperationResultNone, fmt.Errorf("get remote ServiceAccount %s/%s: %w", subject.Namespace, subject.Name, err)
	}

	if sa.GetAnnotations()[selfHostedRoleARNAnnotation] != roleARN {
		return controllerutil.OperationResultNone, nil
	}

	base := sa.DeepCopy()
	annotations := ensureAnnotations(sa)

	for _, key := range selfHostedServiceAccountAnnotationKeys() {
		delete(annotations, key)
	}

	if err := c.Patch(ctx, sa, client.MergeFrom(base)); err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("patch remote ServiceAccount %s/%s: %w", sa.Namespace, sa.Name, err)
	}

	return controllerutil.OperationResultUpdated, nil
}
