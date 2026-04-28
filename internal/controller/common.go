package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

const (
	// transientRequeue is the standard RequeueAfter for retrying a transient
	// failure (inventory not yet ready, remote cluster unavailable, ACK
	// resource still syncing). Tuned to amortize controller cost while keeping
	// recovery latency tolerable.
	transientRequeue = 30 * time.Second

	// channelFullRequeue is the back-off used when a buffered event channel is
	// momentarily full. Shorter than transientRequeue so the dropped event
	// surfaces quickly.
	channelFullRequeue = 5 * time.Second

	// selfHostedSteadyStateRequeue is the long-period safety requeue for
	// SelfHostedIRSA roles whose remote ServiceAccount is already annotated.
	// It guards against missed remote events without storming the API server.
	selfHostedSteadyStateRequeue = time.Hour

	// dependencySteadyStateRequeue periodically rechecks objects that are
	// otherwise idle. This is the retry path for dependency watch map functions,
	// which cannot return errors to controller-runtime when their fan-out List
	// fails.
	dependencySteadyStateRequeue = time.Hour
)

// errReconcileDone signals a reconcile-step helper has already produced the
// desired ctrl.Result and patched status, so the parent reconciler must stop
// walking the pipeline instead of continuing to subsequent steps.
var errReconcileDone = errors.New("reconcile step finished")

func setCondition(conditions *[]metav1.Condition, generation int64, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
	})
}

func setACKReadyCondition(conditions *[]metav1.Condition, generation int64, condType, kind string, ready bool) {
	if ready {
		setCondition(conditions, generation, condType, metav1.ConditionTrue, identityv1.ReasonACKResourceSynced, kind+" is synced by ACK")

		return
	}

	setCondition(conditions, generation, condType, metav1.ConditionFalse, identityv1.ReasonACKResourceNotSynced, "waiting for ACK "+kind+" sync")
}

func failReady(conditions *[]metav1.Condition, generation int64, condType, reason, message string) {
	setCondition(conditions, generation, condType, metav1.ConditionFalse, reason, message)
	setCondition(conditions, generation, identityv1.ConditionReady, metav1.ConditionFalse, reason, message)
}

// loadOperatorConfig returns a defaulted config; callers must not mutate it
// because WithDefaults may return the cached input object unchanged.
func loadOperatorConfig(ctx context.Context, reader client.Reader) (*identityv1.AWSWorkloadIdentityOperatorConfig, error) {
	config := &identityv1.AWSWorkloadIdentityOperatorConfig{}
	if err := reader.Get(ctx, client.ObjectKey{Name: identityv1.DefaultName}, config); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("AWSWorkloadIdentityOperatorConfig/default is required")
		}

		return nil, fmt.Errorf("get AWSWorkloadIdentityOperatorConfig/default: %w", err)
	}

	return config.WithDefaults(), nil
}

// createOrUpdate applies a child object. When owner is non-nil it stamps a
// controller reference (for hub-cluster ACK CRs); when nil it skips the
// reference (for remote workload-cluster objects, where cross-cluster ownerRefs
// are invalid).
func createOrUpdate(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner, obj client.Object, mutate controllerutil.MutateFn) (controllerutil.OperationResult, error) {
	var op controllerutil.OperationResult

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error

		op, err = controllerutil.CreateOrUpdate(ctx, c, obj, func() error {
			if owner != nil {
				if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
					return fmt.Errorf("set owner reference: %w", err)
				}
			}

			return mutate()
		})

		return err //nolint:wrapcheck // outer createOrUpdate wraps with file/name context below
	})
	if err != nil {
		return op, fmt.Errorf("create or update %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}

	return op, nil
}

func ensureFinalizer(ctx context.Context, c client.Client, recorder events.EventRecorder, log logr.Logger, obj client.Object, finalizer string) (bool, error) {
	if !obj.GetDeletionTimestamp().IsZero() {
		return false, nil
	}

	if controllerutil.ContainsFinalizer(obj, finalizer) {
		return false, nil
	}

	base := obj.DeepCopyObject().(client.Object) //nolint:forcetypeassert // DeepCopyObject preserves obj's concrete type, which satisfies client.Object
	controllerutil.AddFinalizer(obj, finalizer)

	if err := c.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("add finalizer on %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}

	log.Info("finalizer added", logKeyOperation, logOpAddFinalizer)
	recordFinalizerAdded(recorder, obj)

	return true, nil
}

func removeFinalizer(ctx context.Context, c client.Client, recorder events.EventRecorder, log logr.Logger, obj client.Object, finalizer string) error {
	if !controllerutil.ContainsFinalizer(obj, finalizer) {
		return nil
	}

	base := obj.DeepCopyObject().(client.Object) //nolint:forcetypeassert // DeepCopyObject preserves obj's concrete type, which satisfies client.Object
	controllerutil.RemoveFinalizer(obj, finalizer)

	if err := c.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("remove finalizer on %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}

	log.Info("finalizer removed", logKeyOperation, logOpRemoveFinalizer)
	recordFinalizerRemoved(recorder, obj)

	return nil
}

func mergeStringMap(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}

	maps.Copy(dst, src)

	return dst
}

func mergeLabels(obj client.Object, src map[string]string) {
	obj.SetLabels(mergeStringMap(obj.GetLabels(), src))
}

// ensureAnnotations returns a map whose mutations are reflected on obj,
// attaching a freshly allocated map when none exists yet.
func ensureAnnotations(obj client.Object) map[string]string {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
		obj.SetAnnotations(annotations)
	}

	return annotations
}

// patchStatusAndObserve writes obj's status using a MergeFrom patch against
// patchBase, then records condition transitions via metrics and events. Callers
// own the early-return-on-no-change check because the status struct shape is
// type-specific.
func patchStatusAndObserve[T client.Object](
	ctx context.Context,
	log logr.Logger,
	statusClient client.SubResourceWriter,
	recorder events.EventRecorder,
	controller string,
	obj, patchBase T,
	beforeConditions, afterConditions []metav1.Condition,
) error {
	if err := statusClient.Patch(ctx, obj, client.MergeFrom(patchBase)); err != nil {
		return fmt.Errorf("patch %T %s/%s status: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}

	transitions := conditionTransitions(beforeConditions, afterConditions)
	observeConditionTransitions(log, controller, transitions)
	recordConditionEvents(recorder, obj, transitions)

	return nil
}

func isDefaultObject(obj client.Object) bool {
	return obj.GetName() == identityv1.DefaultName
}

// deleteAllIgnoreMissing issues Delete for each object and ignores both
// NotFound and NoMatchKind errors. NoMatchKind covers ACK CRDs that may not be
// installed during teardown; standard core objects never trigger it.
func deleteAllIgnoreMissing(ctx context.Context, c client.Client, objs []client.Object) error {
	for _, obj := range objs {
		err := c.Delete(ctx, obj)
		if client.IgnoreNotFound(err) == nil || meta.IsNoMatchError(err) {
			continue
		}

		return fmt.Errorf("delete %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
}

func requestsForList(ctx context.Context, log logr.Logger, c client.Reader, list client.ObjectList, opts ...client.ListOption) []reconcile.Request {
	log = log.WithValues(
		logKeyOperation, logOpWatchMapList,
		logKeyListType, fmt.Sprintf("%T", list),
	)

	if err := c.List(ctx, list, opts...); err != nil {
		log.Error(err, "failed to list objects for watch map")

		return nil
	}

	items, err := meta.ExtractList(list)
	if err != nil {
		log.Error(err, "failed to extract objects for watch map")

		return nil
	}

	requests := make([]reconcile.Request, 0, len(items))

	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			continue
		}

		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
	}

	return requests
}

// watchMapLogger builds the structured logger used by EnqueueRequestsFromMapFunc
// callbacks with the standard awio.map.* / k8s.watch.resource.* keys.
func watchMapLogger(ctx context.Context, controllerName, mapFunc, watchKind string, obj client.Object) logr.Logger {
	return logf.FromContext(ctx).WithValues(
		"controller", controllerName,
		logKeyMapFunc, mapFunc,
		logKeyWatchKind, watchKind,
		logKeyWatchResNS, obj.GetNamespace(),
		logKeyWatchResName, obj.GetName(),
	)
}

// namespacedNameFromString parses a "namespace/name" encoding (the canonical
// form used for ClusterProfile keys, OwnerRef annotations, and resolved cluster
// names) into a NamespacedName. Both halves must be non-empty.
func namespacedNameFromString(value string) (types.NamespacedName, error) {
	namespace, name, ok := strings.Cut(value, "/")
	if !ok || namespace == "" || name == "" {
		return types.NamespacedName{}, fmt.Errorf("expected namespace/name, got %q", value)
	}

	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}
