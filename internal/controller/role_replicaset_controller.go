//nolint:wsl_v5,nlreturn // ReplicaSet reconciliation is a linear state machine; adjacency keeps object mutations readable.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	crbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

const (
	placementAPIGroupOCM  = "cluster.open-cluster-management.io"
	placementKindOCM      = "Placement"
	placementDecisionKind = "PlacementDecision"

	placementAPIVersionOCM = "cluster.open-cluster-management.io/v1beta1"
	placementLabelOCM      = "cluster.open-cluster-management.io/placement"
)

var (
	placementGVKOCM         = schema.GroupVersionKind{Group: placementAPIGroupOCM, Version: "v1beta1", Kind: placementKindOCM}
	placementDecisionGVKOCM = schema.GroupVersionKind{Group: placementAPIGroupOCM, Version: "v1beta1", Kind: placementDecisionKind}
)

// AWSServiceAccountRoleReplicaSetReconciler reconciles fleet role bindings.
//
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountrolereplicasets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountrolereplicasets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountrolereplicasets/finalizers,verbs=update
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=placements;placementdecisions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type AWSServiceAccountRoleReplicaSetReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	Recorder                events.EventRecorder
	MaxConcurrentReconciles int
}

// Reconcile applies per-cluster AWSServiceAccountRole children for a placement.
func (r *AWSServiceAccountRoleReplicaSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	log := loggerForRequest(ctx, metrics.ControllerRoleReplicaSet, req)
	ctx = logf.IntoContext(ctx, log)
	log.V(1).Info("starting reconcile")

	defer func() {
		logReconcileEnd(log, result, reconcileErr)
	}()

	rs := &identityv1.AWSServiceAccountRoleReplicaSet{}
	if err := r.Get(ctx, req.NamespacedName, rs); err != nil {
		if ignored := client.IgnoreNotFound(err); ignored != nil {
			return ctrl.Result{}, fmt.Errorf("get AWSServiceAccountRoleReplicaSet %s: %w", req.NamespacedName, ignored)
		}

		return ctrl.Result{}, nil
	}

	added, err := ensureFinalizer(ctx, r.Client, r.Recorder, log, rs, identityv1.ServiceAccountRoleReplicaSetFinalizer)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		return ctrl.Result{}, nil
	}

	if !rs.DeletionTimestamp.IsZero() {
		return r.reconcileReplicaSetDelete(ctx, log, rs)
	}

	return r.reconcileReplicaSetNormal(ctx, log, rs)
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) reconcileReplicaSetNormal(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet) (ctrl.Result, error) {
	beforeStatus := rs.Status.DeepCopy()
	rs.Status.ObservedGeneration = rs.Generation

	if err := validateReplicaSetTemplateMetadata(rs); err != nil {
		message := err.Error()
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonInvalidSpec, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec, message)

		return ctrl.Result{}, r.patchReplicaSetStatus(ctx, log, rs, beforeStatus)
	}

	selected, resolveErr := r.resolveReplicaSetPlacements(ctx, rs)
	if resolveErr != nil {
		message := resolveErr.Error()
		reason := placementResolveReason(resolveErr)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionPlacementResolved, metav1.ConditionFalse, reason, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, reason, message)

		return ctrl.Result{RequeueAfter: transientRequeue}, r.patchReplicaSetStatus(ctx, log, rs, beforeStatus)
	}

	resetReplicaSetDerivedStatus(rs)

	setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionPlacementResolved, metav1.ConditionTrue, identityv1.ReasonResolved, "placement references resolved")

	rs.Status.SelectedClusterCount = countInt32(len(selected))
	rs.Status.DesiredClusterCount = countInt32(len(selected))

	selectedClusters := map[string]struct{}{}

	for _, clusterName := range selected {
		selectedClusters[clusterName] = struct{}{}
		summary := r.reconcileReplicaSetCluster(ctx, log, rs, clusterName)
		updateReplicaSetCounts(rs, &summary)
		appendReplicaSetSummary(rs, &summary)
	}

	pruned, pruneErr := r.pruneReplicaSetChildren(ctx, rs, selectedClusters)
	if pruneErr != nil {
		message := pruneErr.Error()
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, message)

		return ctrl.Result{}, r.patchReplicaSetStatus(ctx, log, rs, beforeStatus)
	}
	rs.Status.StaleClusterCount += countInt32(pruned)

	setReplicaSetAggregateConditions(rs)

	return ctrl.Result{}, r.patchReplicaSetStatus(ctx, log, rs, beforeStatus)
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) reconcileReplicaSetDelete(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet) (ctrl.Result, error) {
	beforeStatus := rs.Status.DeepCopy()
	children, err := r.listReplicaSetChildren(ctx, rs)
	if err != nil {
		return ctrl.Result{}, err
	}

	remaining := 0
	for i := range children.Items {
		child := &children.Items[i]
		if !replicaSetOwnsChild(rs, child) {
			continue
		}

		remaining++

		if !child.DeletionTimestamp.IsZero() {
			continue
		}

		if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete AWSServiceAccountRole %s/%s: %w", child.Namespace, child.Name, err)
		}
	}

	if remaining > 0 {
		rs.Status.ObservedGeneration = rs.Generation
		message := fmt.Sprintf("waiting for %d generated AWSServiceAccountRole children to be deleted", remaining)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionCleanupBlocked, metav1.ConditionTrue, identityv1.ReasonDeletionBlocked, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonDeletionBlocked, message)
		if err := r.patchReplicaSetStatus(ctx, log, rs, beforeStatus); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	if err := removeFinalizer(ctx, r.Client, rs, identityv1.ServiceAccountRoleReplicaSetFinalizer); err != nil {
		return ctrl.Result{}, err
	}
	recordFinalizerRemoved(r.Recorder, rs)

	return ctrl.Result{}, nil
}

//nolint:cyclop,funlen // The branch sequence mirrors the per-cluster state machine and keeps phase mutations next to the API calls.
func (r *AWSServiceAccountRoleReplicaSetReconciler) reconcileReplicaSetCluster(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet, clusterName string) identityv1.AWSServiceAccountRoleClusterSummary {
	summary := identityv1.AWSServiceAccountRoleClusterSummary{
		ClusterName: clusterName,
		Namespace:   clusterName,
		Name:        rs.Name,
		Phase:       identityv1.AWSServiceAccountRoleClusterPending,
		Reason:      identityv1.ReasonChildrenPending,
		Message:     "waiting for generated AWSServiceAccountRole child readiness",
	}

	if err := r.Get(ctx, client.ObjectKey{Name: clusterName}, &corev1.Namespace{}); err != nil {
		summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
		summary.Reason = identityv1.ReasonClusterNamespaceMissing
		summary.Message = fmt.Sprintf("cluster namespace %q is required", clusterName)

		return summary
	}

	child := &identityv1.AWSServiceAccountRole{}
	key := client.ObjectKey{Namespace: clusterName, Name: rs.Name}
	if err := r.Get(ctx, key, child); err != nil {
		if !apierrors.IsNotFound(err) {
			summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
			summary.Reason = identityv1.ReasonChildApplyFailed
			summary.Message = fmt.Sprintf("get child role: %v", err)

			return summary
		}

		created := buildReplicaSetChild(rs, clusterName)
		if err := r.Create(ctx, created); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return r.reconcileReplicaSetCluster(ctx, log, rs, clusterName)
			}
			summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
			summary.Reason = identityv1.ReasonChildApplyFailed
			summary.Message = fmt.Sprintf("create child role: %v", err)

			return summary
		}

		logChildApply(log, metrics.ControllerRoleReplicaSet, "AWSServiceAccountRole", key.String(), controllerutil.OperationResultCreated, nil)

		return summary
	}

	if !replicaSetOwnsChild(rs, child) {
		summary.Phase = identityv1.AWSServiceAccountRoleClusterConflict
		summary.Reason = identityv1.ReasonChildConflict
		summary.Message = fmt.Sprintf("AWSServiceAccountRole %s exists and is not owned by this ReplicaSet", key.String())

		return summary
	}

	if !sameServiceAccountSubject(child.Spec.ServiceAccount, rs.Spec.Template.Spec.ServiceAccount) {
		if child.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
				summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
				summary.Reason = identityv1.ReasonChildApplyFailed
				summary.Message = fmt.Sprintf("delete stale immutable child role: %v", err)

				return summary
			}
		}

		summary.Reason = identityv1.ReasonImmutableChildDrift
		summary.Message = "waiting for stale child role with immutable serviceAccount drift to be deleted"

		return summary
	}

	before := child.DeepCopy()
	applyReplicaSetChild(rs, child)
	if !apiequality.Semantic.DeepEqual(before.Labels, child.Labels) ||
		!apiequality.Semantic.DeepEqual(before.Annotations, child.Annotations) ||
		!apiequality.Semantic.DeepEqual(before.Spec, child.Spec) {
		if err := r.Patch(ctx, child, client.MergeFrom(before)); err != nil {
			summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
			summary.Reason = identityv1.ReasonChildApplyFailed
			summary.Message = fmt.Sprintf("patch child role: %v", err)

			return summary
		}

		logChildApply(log, metrics.ControllerRoleReplicaSet, "AWSServiceAccountRole", key.String(), controllerutil.OperationResultUpdated, nil)
	}

	if childReady(child) {
		summary.Phase = identityv1.AWSServiceAccountRoleClusterReady
		summary.Ready = true
		summary.Reason = identityv1.ReasonReady
		summary.Message = "generated AWSServiceAccountRole child is ready"
	}

	return summary
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) pruneReplicaSetChildren(ctx context.Context, rs *identityv1.AWSServiceAccountRoleReplicaSet, selected map[string]struct{}) (int, error) {
	children, err := r.listReplicaSetChildren(ctx, rs)
	if err != nil {
		return 0, err
	}

	pruned := 0
	for i := range children.Items {
		child := &children.Items[i]
		if !replicaSetOwnsChild(rs, child) {
			continue
		}

		if _, ok := selected[child.Namespace]; ok {
			continue
		}

		if !child.DeletionTimestamp.IsZero() {
			pruned++

			continue
		}

		if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
			return pruned, fmt.Errorf("delete stale AWSServiceAccountRole %s/%s: %w", child.Namespace, child.Name, err)
		}

		pruned++
	}

	return pruned, nil
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) listReplicaSetChildren(ctx context.Context, rs *identityv1.AWSServiceAccountRoleReplicaSet) (*identityv1.AWSServiceAccountRoleList, error) {
	children := &identityv1.AWSServiceAccountRoleList{}
	if err := r.List(ctx, children, roleByReplicaSetUIDKey(string(rs.UID))); err != nil {
		return nil, fmt.Errorf("list AWSServiceAccountRoles by ReplicaSet UID %q: %w", rs.UID, err)
	}

	return children, nil
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) resolveReplicaSetPlacements(ctx context.Context, rs *identityv1.AWSServiceAccountRoleReplicaSet) ([]string, error) {
	selected := map[string]struct{}{}
	for _, ref := range rs.Spec.PlacementRefs {
		switch {
		case ref.APIGroup == placementAPIGroupOCM && ref.Kind == placementKindOCM:
			clusters, err := r.resolveOCMPlacement(ctx, rs.Namespace, ref.Name)
			if err != nil {
				return nil, err
			}

			for _, clusterName := range clusters {
				selected[clusterName] = struct{}{}
			}
		default:
			return nil, unsupportedPlacementRefError{ref: ref}
		}
	}

	return slices.Sorted(maps.Keys(selected)), nil
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) resolveOCMPlacement(ctx context.Context, namespace, name string) ([]string, error) {
	placement := newUnstructured(placementGVKOCM)
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, placement); err != nil {
		return nil, fmt.Errorf("resolve OCM Placement %s/%s: %w", namespace, name, err)
	}

	decisions := &unstructured.UnstructuredList{}
	decisions.SetAPIVersion(placementAPIVersionOCM)
	decisions.SetKind(placementDecisionKind + "List")
	if err := r.List(ctx, decisions, client.InNamespace(namespace), client.MatchingLabels{placementLabelOCM: name}); err != nil {
		return nil, fmt.Errorf("list OCM PlacementDecisions for %s/%s: %w", namespace, name, err)
	}

	selected := map[string]struct{}{}
	for i := range decisions.Items {
		decision := &decisions.Items[i]
		if !placementOwnsDecision(placement, decision) {
			continue
		}

		clusterNames, err := clusterNamesFromOCMPlacementDecision(decision)
		if err != nil {
			return nil, err
		}

		for _, clusterName := range clusterNames {
			selected[clusterName] = struct{}{}
		}
	}

	return slices.Sorted(maps.Keys(selected)), nil
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) patchReplicaSetStatus(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet, beforeStatus *identityv1.AWSServiceAccountRoleReplicaSetStatus) error {
	if apiequality.Semantic.DeepEqual(*beforeStatus, rs.Status) {
		return nil
	}

	patchBase := rs.DeepCopy()
	patchBase.Status = *beforeStatus

	return patchStatusAndObserve(ctx, log, r.Status(), r.Recorder, metrics.ControllerRoleReplicaSet, rs, patchBase, beforeStatus.Conditions, rs.Status.Conditions)
}

// SetupWithManager registers the reconciler with a controller manager.
func (r *AWSServiceAccountRoleReplicaSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&identityv1.AWSServiceAccountRoleReplicaSet{}, crbuilder.WithPredicates(rootObjectChangedPredicate(metrics.ControllerRoleReplicaSet))).
		Watches(&identityv1.AWSServiceAccountRole{}, handler.EnqueueRequestsFromMapFunc(r.replicaSetsForChildRole)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles})

	if hasMapping(mgr, placementGVKOCM) && hasMapping(mgr, placementDecisionGVKOCM) {
		builder = builder.
			Watches(newUnstructured(placementGVKOCM), handler.EnqueueRequestsFromMapFunc(r.replicaSetsForOCMPlacement)).
			Watches(newUnstructured(placementDecisionGVKOCM), handler.EnqueueRequestsFromMapFunc(r.replicaSetsForOCMPlacementDecision))
	}

	if err := builder.Complete(r); err != nil {
		return fmt.Errorf("set up AWSServiceAccountRoleReplicaSet controller: %w", err)
	}

	return nil
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) replicaSetsForChildRole(ctx context.Context, obj client.Object) []reconcile.Request {
	if ownerRef := obj.GetAnnotations()[identityv1.AnnotationReplicaSetOwnerRef]; ownerRef != "" {
		if namespace, name, ok := strings.Cut(ownerRef, "/"); ok && namespace != "" && name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
		}
	}

	log := watchMapLogger(ctx, metrics.ControllerRoleReplicaSet, "replicaSetsForChildRole", "AWSServiceAccountRole", obj)
	replicaSets := &identityv1.AWSServiceAccountRoleReplicaSetList{}
	requests := requestsForList(ctx, log, r.Client, replicaSets)
	filtered := requests[:0]

	for _, req := range requests {
		if req.Name == obj.GetName() {
			filtered = append(filtered, req)
		}
	}

	return filtered
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) replicaSetsForOCMPlacement(ctx context.Context, obj client.Object) []reconcile.Request {
	log := watchMapLogger(ctx, metrics.ControllerRoleReplicaSet, "replicaSetsForOCMPlacement", "Placement", obj)

	return requestsForReplicaSetsReferencingPlacement(ctx, log, r.Client, obj.GetNamespace(), placementAPIGroupOCM, placementKindOCM, obj.GetName())
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) replicaSetsForOCMPlacementDecision(ctx context.Context, obj client.Object) []reconcile.Request {
	placementName := obj.GetLabels()[placementLabelOCM]
	if placementName == "" {
		return nil
	}

	log := watchMapLogger(ctx, metrics.ControllerRoleReplicaSet, "replicaSetsForOCMPlacementDecision", "PlacementDecision", obj)

	return requestsForReplicaSetsReferencingPlacement(ctx, log, r.Client, obj.GetNamespace(), placementAPIGroupOCM, placementKindOCM, placementName)
}

func requestsForReplicaSetsReferencingPlacement(ctx context.Context, log logr.Logger, c client.Reader, namespace, apiGroup, kind, name string) []reconcile.Request {
	replicaSets := &identityv1.AWSServiceAccountRoleReplicaSetList{}
	if err := c.List(ctx, replicaSets, client.InNamespace(namespace)); err != nil {
		log.Error(err, "failed to list ReplicaSets for placement watch map")
		return nil
	}

	requests := make([]reconcile.Request, 0, len(replicaSets.Items))
	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		if replicaSetReferencesPlacement(rs, apiGroup, kind, name) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rs)})
		}
	}

	return requests
}

func replicaSetReferencesPlacement(rs *identityv1.AWSServiceAccountRoleReplicaSet, apiGroup, kind, name string) bool {
	for _, ref := range rs.Spec.PlacementRefs {
		if ref.APIGroup == apiGroup && ref.Kind == kind && ref.Name == name {
			return true
		}
	}

	return false
}

func buildReplicaSetChild(rs *identityv1.AWSServiceAccountRoleReplicaSet, clusterName string) *identityv1.AWSServiceAccountRole {
	child := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name,
			Namespace: clusterName,
		},
	}
	applyReplicaSetChild(rs, child)

	return child
}

func applyReplicaSetChild(rs *identityv1.AWSServiceAccountRoleReplicaSet, child *identityv1.AWSServiceAccountRole) {
	labels := map[string]string{}
	annotations := map[string]string{}

	if rs.Spec.Template.Metadata != nil {
		maps.Copy(labels, rs.Spec.Template.Metadata.Labels)
		maps.Copy(annotations, rs.Spec.Template.Metadata.Annotations)
	}

	labels[identityv1.LabelManagedBy] = identityv1.ManagedByValue
	labels[identityv1.LabelReplicaSetUID] = string(rs.UID)
	annotations[identityv1.AnnotationReplicaSetOwnerRef] = client.ObjectKeyFromObject(rs).String()

	child.Labels = labels
	child.Annotations = annotations
	child.Spec = *rs.Spec.Template.Spec.DeepCopy()
}

func replicaSetOwnsChild(rs *identityv1.AWSServiceAccountRoleReplicaSet, child *identityv1.AWSServiceAccountRole) bool {
	labels := child.GetLabels()

	return labels[identityv1.LabelManagedBy] == identityv1.ManagedByValue &&
		labels[identityv1.LabelReplicaSetUID] == string(rs.UID)
}

func childReady(child *identityv1.AWSServiceAccountRole) bool {
	condition := meta.FindStatusCondition(child.Status.Conditions, identityv1.ConditionReady)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return false
	}

	return condition.ObservedGeneration == child.Generation
}

func countInt32(n int) int32 {
	const maxInt32 = 2147483647

	if n > maxInt32 {
		return maxInt32
	}

	return int32(n) //nolint:gosec // bounded above by maxInt32 before conversion
}

func resetReplicaSetDerivedStatus(rs *identityv1.AWSServiceAccountRoleReplicaSet) {
	rs.Status = identityv1.AWSServiceAccountRoleReplicaSetStatus{
		ObservedGeneration: rs.Status.ObservedGeneration,
		Conditions:         rs.Status.Conditions,
	}
}

func updateReplicaSetCounts(rs *identityv1.AWSServiceAccountRoleReplicaSet, summary *identityv1.AWSServiceAccountRoleClusterSummary) {
	switch summary.Phase {
	case identityv1.AWSServiceAccountRoleClusterReady:
		rs.Status.AppliedClusterCount++
		rs.Status.ReadyClusterCount++
	case identityv1.AWSServiceAccountRoleClusterPending:
		rs.Status.AppliedClusterCount++
		if summary.Reason == identityv1.ReasonImmutableChildDrift {
			rs.Status.StaleClusterCount++
		}
	case identityv1.AWSServiceAccountRoleClusterConflict:
		rs.Status.ConflictCount++
	case identityv1.AWSServiceAccountRoleClusterFailed:
		rs.Status.FailureCount++
	}
}

func appendReplicaSetSummary(rs *identityv1.AWSServiceAccountRoleReplicaSet, summary *identityv1.AWSServiceAccountRoleClusterSummary) {
	if len(rs.Status.Clusters) < 100 {
		rs.Status.Clusters = append(rs.Status.Clusters, *summary)
	}

	if summary.Phase != identityv1.AWSServiceAccountRoleClusterFailed && summary.Phase != identityv1.AWSServiceAccountRoleClusterConflict {
		return
	}

	if len(rs.Status.FailedClusters) >= 50 {
		return
	}

	rs.Status.FailedClusters = append(rs.Status.FailedClusters, identityv1.AWSServiceAccountRoleClusterFailure{
		ClusterName: summary.ClusterName,
		Phase:       summary.Phase,
		Reason:      summary.Reason,
		Message:     summary.Message,
	})
}

func setReplicaSetAggregateConditions(rs *identityv1.AWSServiceAccountRoleReplicaSet) {
	switch {
	case rs.Status.FailureCount > 0:
		message := fmt.Sprintf("%d selected clusters failed to apply", rs.Status.FailureCount)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, message)
	case rs.Status.ConflictCount > 0:
		message := fmt.Sprintf("%d selected clusters have conflicting AWSServiceAccountRole children", rs.Status.ConflictCount)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonChildConflict, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildConflict, message)
	default:
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionTrue, identityv1.ReasonChildrenApplied, "generated AWSServiceAccountRole children are applied")
	}

	if rs.Status.ReadyClusterCount == rs.Status.DesiredClusterCount && rs.Status.FailureCount == 0 && rs.Status.ConflictCount == 0 && rs.Status.StaleClusterCount == 0 {
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionTrue, identityv1.ReasonChildrenReady, "generated AWSServiceAccountRole children are ready")
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "ReplicaSet is ready")
		return
	}

	message := fmt.Sprintf("%d of %d generated AWSServiceAccountRole children are ready", rs.Status.ReadyClusterCount, rs.Status.DesiredClusterCount)
	setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending, message)
	// Don't clobber a more-specific Ready=False already written by failure/conflict branches above.
	if rs.Status.FailureCount == 0 && rs.Status.ConflictCount == 0 {
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending, message)
	}
}

func validateReplicaSetTemplateMetadata(rs *identityv1.AWSServiceAccountRoleReplicaSet) error {
	if rs.Spec.Template.Metadata == nil {
		return nil
	}

	if key := firstReservedKey(rs.Spec.Template.Metadata.Labels, reservedReplicaSetTemplateLabels()); key != "" {
		return fmt.Errorf("spec.template.metadata.labels[%q] is reserved", key)
	}

	if key := firstReservedKey(rs.Spec.Template.Metadata.Annotations, reservedReplicaSetTemplateAnnotations()); key != "" {
		return fmt.Errorf("spec.template.metadata.annotations[%q] is reserved", key)
	}

	return nil
}

func firstReservedKey(values map[string]string, reserved map[string]struct{}) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	for _, key := range keys {
		if _, ok := reserved[key]; ok {
			return key
		}
	}

	return ""
}

func reservedReplicaSetTemplateLabels() map[string]struct{} {
	return map[string]struct{}{
		identityv1.LabelManagedBy:      {},
		identityv1.LabelConfigUID:      {},
		identityv1.LabelBindingUID:     {},
		identityv1.LabelInventoryNS:    {},
		identityv1.LabelOwnerRef:       {},
		identityv1.LabelServiceAccount: {},
		identityv1.LabelDelivery:       {},
		identityv1.LabelRuntime:        {},
		identityv1.LabelReplicaSetUID:  {},
	}
}

func reservedReplicaSetTemplateAnnotations() map[string]struct{} {
	return map[string]struct{}{
		identityv1.AnnotationReplicaSetOwnerRef: {},
	}
}

func placementOwnsDecision(placement, decision *unstructured.Unstructured) bool {
	for _, ref := range decision.GetOwnerReferences() {
		if ref.APIVersion == placementAPIVersionOCM &&
			ref.Kind == placementKindOCM &&
			ref.Name == placement.GetName() &&
			ref.UID == placement.GetUID() {
			return true
		}
	}

	return false
}

func clusterNamesFromOCMPlacementDecision(decision *unstructured.Unstructured) ([]string, error) {
	rawDecisions, ok, err := unstructured.NestedSlice(decision.Object, "status", "decisions")
	if err != nil {
		return nil, fmt.Errorf("read OCM PlacementDecision %s/%s decisions: %w", decision.GetNamespace(), decision.GetName(), err)
	}
	if !ok {
		return nil, nil
	}

	clusters := make([]string, 0, len(rawDecisions))
	for _, raw := range rawDecisions {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid OCM PlacementDecision %s/%s status.decisions entry", decision.GetNamespace(), decision.GetName())
		}
		clusterName, _ := entry["clusterName"].(string)
		if clusterName == "" {
			continue
		}
		clusters = append(clusters, clusterName)
	}

	return clusters, nil
}

func newUnstructured(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	return obj
}

func hasMapping(mgr ctrl.Manager, gvk schema.GroupVersionKind) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)

	return err == nil
}

type unsupportedPlacementRefError struct {
	ref identityv1.PlacementReference
}

func (e unsupportedPlacementRefError) Error() string {
	return fmt.Sprintf("unsupported placement reference %s/%s %q", e.ref.APIGroup, e.ref.Kind, e.ref.Name)
}

func placementResolveReason(err error) string {
	var unsupported unsupportedPlacementRefError
	if errors.As(err, &unsupported) {
		return identityv1.ReasonPlacementUnsupported
	}

	return identityv1.ReasonPlacementUnavailable
}
