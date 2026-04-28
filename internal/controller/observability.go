package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

const (
	eventActionAddFinalizer          = "AddFinalizer"
	eventActionRemoveFinalizer       = "RemoveFinalizer"
	eventActionRepairAnnotation      = "RepairAnnotation"
	eventActionConditionTransitioned = "ConditionTransitioned"

	eventReasonFinalizerAdded     = "FinalizerAdded"
	eventReasonFinalizerRemoved   = "FinalizerRemoved"
	eventReasonAnnotationRepaired = "AnnotationRepaired"
)

func loggerForRequest(ctx context.Context, controller string, req ctrl.Request) logr.Logger {
	return logf.FromContext(ctx).WithValues(
		"controller", controller,
		"k8s.resource.group", identityv1.GroupVersion.Group,
		"k8s.resource.kind", controller,
		"k8s.resource.namespace", req.Namespace,
		"k8s.resource.name", req.Name,
	)
}

func loggerForRemoteRequest(ctx context.Context, controller string, req mcreconcile.Request, inventoryNamespace string) logr.Logger {
	return logf.FromContext(ctx).WithValues(
		"controller", controller,
		"awio.cluster.name", req.ClusterName.String(),
		"awio.inventory.namespace", inventoryNamespace,
		"k8s.resource.namespace", req.Namespace,
		"k8s.resource.name", req.Name,
	)
}

// loggerForSelfHostedRemoteRequest is loggerForRemoteRequest pre-tagged with
// the SelfHostedIRSA delivery type. The three self-hosted remote reconcilers
// share this prefix.
func loggerForSelfHostedRemoteRequest(ctx context.Context, controller string, req mcreconcile.Request, inventoryNamespace string) logr.Logger {
	return loggerForRemoteRequest(ctx, controller, req, inventoryNamespace).
		WithValues("awio.delivery.type", string(identityv1.DeliveryTypeSelfHostedIRSA))
}

// logReconcileEnd records requeue_after and outcome at V(1); controller-runtime
// already logs the error itself, so we only attach it to the structured line.
func logReconcileEnd(log logr.Logger, result ctrl.Result, err error) {
	log = log.WithValues("requeue_after", result.RequeueAfter.String())

	if err != nil {
		log.V(1).Info("finished reconcile with error", "error", err.Error())

		return
	}

	log.V(1).Info("finished reconcile")
}

func logChildApply(log logr.Logger, controller, childKind, childName string, operation controllerutil.OperationResult, err error) {
	metrics.RecordChildApply(controller, childKind, operation, err)
	logApplyOutcome(log, "child", childKind, childName, operation, err)
}

func logRemoteApply(log logr.Logger, resource, name string, operation controllerutil.OperationResult, err error) {
	deliveryType := string(identityv1.DeliveryTypeSelfHostedIRSA)

	result := metrics.RemoteDeliveryResultSuccess
	reason := string(operation)

	if err != nil {
		result = metrics.RemoteDeliveryResultError
		reason = metrics.RemoteDeliveryReasonApplyFailed
	}

	metrics.RecordRemoteDelivery(deliveryType, resource, result, reason)
	logApplyOutcome(log.WithValues("awio.delivery.type", deliveryType), "remote", resource, name, operation, err)
}

func logApplyOutcome(log logr.Logger, scope, kind, name string, operation controllerutil.OperationResult, err error) {
	log = log.WithValues(
		"awio.child.kind", kind,
		"awio.child.name", name,
		"awio.operation", string(operation),
	)

	switch {
	case err != nil:
		log.Error(err, scope+" resource apply failed")
	case operation == controllerutil.OperationResultNone:
		log.V(1).Info(scope + " resource unchanged")
	default:
		log.Info(scope + " resource applied")
	}
}

func conditionTransitions(before, after []metav1.Condition) []metav1.Condition {
	var changes []metav1.Condition

	for i := range after {
		next := after[i]

		previous := meta.FindStatusCondition(before, next.Type)
		if previous == nil || previous.Status != next.Status || previous.Reason != next.Reason {
			changes = append(changes, next)
		}
	}

	return changes
}

func observeConditionTransitions(log logr.Logger, kind string, transitions []metav1.Condition) {
	for i := range transitions {
		condition := &transitions[i]
		metrics.RecordConditionTransition(kind, condition)

		level := 1
		if condition.Type == identityv1.ConditionReady ||
			condition.Type == identityv1.ConditionDeliveryReady ||
			condition.Type == identityv1.ConditionIssuerReady {
			level = 0
		}

		log.V(level).Info("condition transitioned",
			"awio.condition.type", condition.Type,
			"awio.condition.status", string(condition.Status),
			"awio.condition.reason", condition.Reason,
		)
	}
}

func recordNormalEvent(recorder events.EventRecorder, obj client.Object, reason, action, message string) {
	if recorder == nil {
		return
	}

	recorder.Eventf(obj, nil, corev1.EventTypeNormal, reason, action, message)
}

func recordFinalizerAdded(recorder events.EventRecorder, obj client.Object) {
	recordNormalEvent(recorder, obj, eventReasonFinalizerAdded, eventActionAddFinalizer, "added finalizer for cleanup")
}

func recordFinalizerRemoved(recorder events.EventRecorder, obj client.Object) {
	recordNormalEvent(recorder, obj, eventReasonFinalizerRemoved, eventActionRemoveFinalizer, "removed finalizer after cleanup")
}

func recordAnnotationRepaired(recorder events.EventRecorder, obj client.Object) {
	recordNormalEvent(recorder, obj, eventReasonAnnotationRepaired, eventActionRepairAnnotation, "repaired remote ServiceAccount annotations")
}

func recordConditionEvents(recorder events.EventRecorder, obj client.Object, transitions []metav1.Condition) {
	if recorder == nil {
		return
	}

	for i := range transitions {
		condition := &transitions[i]

		eventType, reason, ok := eventForCondition(condition)
		if !ok {
			continue
		}

		recorder.Eventf(obj, nil, eventType, reason, eventActionConditionTransitioned, eventMessage(condition))
	}
}

func eventForCondition(condition *metav1.Condition) (string, string, bool) {
	if reason, ok := warningReason(condition); ok {
		return corev1.EventTypeWarning, reason, true
	}

	if eventIsACKWaiting(condition) {
		return corev1.EventTypeNormal, identityv1.ReasonACKResourceWaiting, true
	}

	switch condition.Type {
	case identityv1.ConditionDeliveryReady:
		if condition.Status == metav1.ConditionTrue {
			return corev1.EventTypeNormal, identityv1.ReasonHubResourcesReady, true
		}
	case identityv1.ConditionReady:
		if condition.Status == metav1.ConditionTrue {
			return corev1.EventTypeNormal, identityv1.ReasonReady, true
		}
	}

	return "", "", false
}

var warningReasons = sets.New[string](
	identityv1.ReasonInvalidSpec,
	identityv1.ReasonOperatorConfigUnavailable,
	identityv1.ReasonIssuerReconcileFailed,
	identityv1.ReasonOIDCObjectsPublishFailed,
)

func warningReason(condition *metav1.Condition) (string, bool) {
	if condition.Type == identityv1.ConditionDeletionBlocked && condition.Status == metav1.ConditionTrue {
		return identityv1.ReasonDeletionBlocked, true
	}

	if warningReasons.Has(condition.Reason) {
		return condition.Reason, true
	}

	if (condition.Type == identityv1.ConditionInventoryResolved || condition.Type == identityv1.ConditionClusterProfileResolved) && condition.Status != metav1.ConditionTrue {
		return identityv1.ReasonInventoryUnavailable, true
	}

	return "", false
}

func eventIsACKWaiting(condition *metav1.Condition) bool {
	if condition.Type != identityv1.ConditionReady &&
		condition.Type != identityv1.ConditionDeliveryReady &&
		condition.Type != identityv1.ConditionIssuerReady {
		return false
	}

	return condition.Reason == identityv1.ReasonWaitingForACK ||
		condition.Reason == identityv1.ReasonACKResourceNotSynced
}

func eventMessage(condition *metav1.Condition) string {
	return fmt.Sprintf("%s transitioned to %s with reason %s", condition.Type, condition.Status, condition.Reason)
}
