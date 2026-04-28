package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

// SelfHostedServiceAccountReconciler keeps remote workload ServiceAccount
// annotations in sync for self-hosted IRSA.
type SelfHostedServiceAccountReconciler struct {
	LocalClient client.Client
	MCManager   mcmanager.Manager
	Resolver    inventory.Resolver
}

// Reconcile repairs aws-pod-identity-webhook annotations when a remote
// workload ServiceAccount changes.
func (r *SelfHostedServiceAccountReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (result ctrl.Result, reconcileErr error) {
	namespace := inventoryNamespaceFromCluster(req.ClusterName.String())
	log := loggerForSelfHostedRemoteRequest(ctx, metrics.ControllerSelfHostedServiceAccount, req, namespace)
	ctx = logf.IntoContext(ctx, log)
	log.V(1).Info("starting reconcile")

	defer func() {
		logReconcileEnd(log, result, reconcileErr)
	}()

	if namespace == "" {
		metrics.RecordRemoteDelivery("", metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonNoNamespace)

		return ctrl.Result{}, nil
	}

	target, result, err := resolveSelfHostedTarget(ctx, &selfHostedTargetRequest{
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

	return ctrl.Result{}, r.reconcileRemoteServiceAccount(ctx, log, target.GetClient(), namespace, req.NamespacedName)
}

func (r *SelfHostedServiceAccountReconciler) reconcileRemoteServiceAccount(ctx context.Context, log logr.Logger, remoteClient client.Client, inventoryNamespace string, key types.NamespacedName) error {
	current := &corev1.ServiceAccount{}
	if err := remoteClient.Get(ctx, key, current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("get remote ServiceAccount %s: %w", key, err)
	}

	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := r.LocalClient.List(ctx, roles, client.InNamespace(inventoryNamespace), roleByServiceAccountKey(key.Namespace, key.Name), client.Limit(1)); err != nil {
		return fmt.Errorf("list AWSServiceAccountRoles by ServiceAccount index in namespace %q: %w", inventoryNamespace, err)
	}

	if len(roles.Items) == 0 || roles.Items[0].Status.RoleARN == "" {
		return nil
	}

	op, err := patchRemoteServiceAccountObjectAnnotations(ctx, remoteClient, current, roles.Items[0].Status.RoleARN)
	logRemoteApply(log, metrics.ResourceServiceAccount, key.String(), op, err)

	if err != nil {
		return fmt.Errorf("patch remote ServiceAccount annotations %s: %w", key, err)
	}

	return nil
}

// SetupSelfHostedServiceAccountController registers the remote ServiceAccount
// annotation repair controller.
func SetupSelfHostedServiceAccountController(mcMgr mcmanager.Manager, localClient client.Client, resolver inventory.Resolver) error {
	if err := mcbuilder.ControllerManagedBy(mcMgr).
		Named(metrics.ControllerSelfHostedServiceAccount).
		For(&corev1.ServiceAccount{}, mcbuilder.WithPredicates(annotationRepairPredicate())).
		Complete(&SelfHostedServiceAccountReconciler{
			LocalClient: localClient,
			MCManager:   mcMgr,
			Resolver:    resolver,
		}); err != nil {
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

	base := sa.DeepCopy()
	annotations := ensureAnnotations(sa)
	changed := false

	for key, value := range desired {
		if annotations[key] == value {
			continue
		}

		annotations[key] = value
		changed = true
	}

	if !changed {
		return controllerutil.OperationResultNone, nil
	}

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
