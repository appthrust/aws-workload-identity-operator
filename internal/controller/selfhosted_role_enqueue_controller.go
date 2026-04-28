package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

// SelfHostedRoleEnqueueController translates annotated remote ServiceAccount
// deletes into hub-local AWSServiceAccountRole reconcile requests.
type SelfHostedRoleEnqueueController struct {
	LocalClient             client.Client
	RoleEvents              chan<- event.TypedGenericEvent[*identityv1.AWSServiceAccountRole]
	MaxConcurrentReconciles int
}

// Reconcile looks up hub roles bound to the remote ServiceAccount and enqueues
// matching roles on the local role controller.
func (r *SelfHostedRoleEnqueueController) Reconcile(ctx context.Context, req mcreconcile.Request) (result ctrl.Result, reconcileErr error) {
	inventoryNs := inventoryNamespaceFromCluster(req.ClusterName.String())
	log := loggerForSelfHostedRemoteRequest(ctx, metrics.ControllerSelfHostedRoleEnqueue, req, inventoryNs)
	ctx = logf.IntoContext(ctx, log)
	log.V(1).Info("starting reconcile")

	defer func() {
		logReconcileEnd(log, result, reconcileErr)
	}()

	if inventoryNs == "" {
		metrics.RecordRemoteDelivery("", metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonNoNamespace)

		return ctrl.Result{}, nil
	}

	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := r.LocalClient.List(ctx, roles, client.InNamespace(inventoryNs), roleByServiceAccountKey(req.Namespace, req.Name)); err != nil {
		metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultError, metrics.RemoteDeliveryReasonIndexLookupFailed)

		return ctrl.Result{}, fmt.Errorf("list AWSServiceAccountRoles by ServiceAccount index in %q: %w", inventoryNs, err)
	}

	for i := range roles.Items {
		evt := event.TypedGenericEvent[*identityv1.AWSServiceAccountRole]{Object: &roles.Items[i]}
		select {
		case r.RoleEvents <- evt:
			metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSuccess, metrics.RemoteDeliveryReasonEnqueued)
		case <-ctx.Done():
			return ctrl.Result{}, fmt.Errorf("enqueue role event cancelled: %w", ctx.Err())
		default:
			// Channel-full back-off: the role controller's hour-long safety
			// requeue eventually catches missed events, but channelFullRequeue
			// recovers far quicker.
			metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonChannelFull)

			return ctrl.Result{RequeueAfter: channelFullRequeue}, nil
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the annotated remote ServiceAccount delete
// enqueue controller.
func (r *SelfHostedRoleEnqueueController) SetupWithManager(mcMgr mcmanager.Manager) error {
	if err := mcbuilder.ControllerManagedBy(mcMgr).
		Named(metrics.ControllerSelfHostedRoleEnqueue).
		For(&corev1.ServiceAccount{}, mcbuilder.WithPredicates(annotatedServiceAccountDeletePredicate())).
		WithOptions(mccontroller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r); err != nil {
		return fmt.Errorf("set up self-hosted role enqueue controller: %w", err)
	}

	return nil
}

func annotatedServiceAccountDeletePredicate() predicate.Predicate {
	return metricsRecordingPredicate(metrics.ControllerSelfHostedRoleEnqueue, predicateKeepFns{
		Delete: func(e event.DeleteEvent) bool {
			return hasSelfHostedRoleARNAnnotation(e.Object)
		},
	})
}
