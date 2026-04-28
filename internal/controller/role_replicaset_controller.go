//nolint:wsl_v5,nlreturn // ReplicaSet reconciliation is a linear state machine; adjacency keeps object mutations readable.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	clustersdkv1alpha1 "open-cluster-management.io/sdk-go/pkg/apis/cluster/v1alpha1"
	clustersdkv1beta1 "open-cluster-management.io/sdk-go/pkg/apis/cluster/v1beta1"
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
	messageGeneratedChildReady      = "generated AWSServiceAccountRole child is ready"
	messageWaitingForChildReadiness = "waiting for generated AWSServiceAccountRole child readiness"
	messageWaitingForRolloutToApply = "waiting for rollout strategy to apply generated AWSServiceAccountRole child"

	// Status capping bounds keep the on-disk Status object size predictable
	// even when a ReplicaSet selects thousands of clusters; entries beyond
	// these caps are observable only via metrics.
	maxReportedClusters       = 100
	maxReportedFailedClusters = 50
)

var (
	ocmClusterGroupVersion  = schema.GroupVersion{Group: clusterv1beta1.GroupName, Version: "v1beta1"}
	placementGVKOCM         = ocmClusterGroupVersion.WithKind("Placement")
	placementDecisionGVKOCM = ocmClusterGroupVersion.WithKind("PlacementDecision")
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
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
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

//nolint:funlen // Linear pipeline: validate, resolve, list, plan, apply, prune, status — each step is a distinct phase that must run in order.
func (r *AWSServiceAccountRoleReplicaSetReconciler) reconcileReplicaSetNormal(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet) (ctrl.Result, error) {
	beforeStatus := rs.Status.DeepCopy()
	rs.Status.ObservedGeneration = rs.Generation

	if err := validateReplicaSetTemplateMetadata(rs); err != nil {
		message := err.Error()
		failReady(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, identityv1.ReasonInvalidSpec, message)

		return ctrl.Result{}, r.patchReplicaSetStatus(ctx, log, rs, beforeStatus)
	}

	resolution, resolveErr := r.resolveReplicaSetPlacements(ctx, rs)
	if resolveErr != nil {
		message := resolveErr.Error()
		rs.Status.Placements = resolution.placementStatuses
		failReady(&rs.Status.Conditions, rs.Generation, identityv1.ConditionPlacementResolved, identityv1.ReasonPlacementUnavailable, message)

		if patchErr := r.patchReplicaSetStatus(ctx, log, rs, beforeStatus); patchErr != nil {
			return ctrl.Result{}, patchErr
		}

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	children, err := r.listOwnedReplicaSetChildren(ctx, rs)
	if err != nil {
		return ctrl.Result{}, err
	}
	childrenByNS := make(map[string]*identityv1.AWSServiceAccountRole, len(children))
	for _, child := range children {
		childrenByNS[child.Namespace] = child
	}
	template := newReplicaSetChildTemplate(rs)

	resetReplicaSetDerivedStatus(rs)

	setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionPlacementResolved, metav1.ConditionTrue, identityv1.ReasonResolved, "placement references resolved")

	rs.Status.SelectedClusterCount = countInt32(resolution.clusters.Len())
	rs.Status.DesiredClusterCount = countInt32(resolution.clusters.Len())
	rs.Status.Placements = resolution.placementStatuses
	rs.Status.Rollout = beforeStatus.Rollout

	rollout, rolloutErr := r.planReplicaSetRollout(ctx, rs, resolution, childrenByNS, template)
	if rolloutErr != nil {
		message := rolloutErr.Error()
		failReady(&rs.Status.Conditions, rs.Generation, identityv1.ConditionPlacementRolledOut, identityv1.ReasonChildApplyFailed, message)

		return ctrl.Result{}, r.patchReplicaSetStatus(ctx, log, rs, beforeStatus)
	}
	applyReplicaSetRolloutStatus(rs, rollout)

	r.applyReplicaSetRolloutClusters(ctx, log, rs, resolution, rollout, childrenByNS, template)
	rs.Status.StaleClusterCount += countInt32(r.pruneReplicaSetChildren(ctx, children, resolution.clusters))

	setReplicaSetAggregateConditions(rs)

	result := ctrl.Result{}
	if rollout.requeueAfter > 0 {
		result.RequeueAfter = rollout.requeueAfter
	}

	return result, r.patchReplicaSetStatus(ctx, log, rs, beforeStatus)
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) applyReplicaSetRolloutClusters(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet, resolution replicaSetPlacementResolution, rollout replicaSetRolloutPlan, childrenByNS map[string]*identityv1.AWSServiceAccountRole, template *replicaSetChildTemplate) {
	for _, clusterName := range sets.List(resolution.clusters) {
		summary := rollout.summaries[clusterName]
		if rollout.rolloutClusters.Has(clusterName) {
			summary = r.applyReplicaSetCluster(ctx, log, rs, clusterName, summary, childrenByNS[clusterName], template)
		}
		updateReplicaSetCounts(rs, &summary)
		appendReplicaSetSummary(rs, &summary)
	}
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) reconcileReplicaSetDelete(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet) (ctrl.Result, error) {
	beforeStatus := rs.Status.DeepCopy()
	children, err := r.listOwnedReplicaSetChildren(ctx, rs)
	if err != nil {
		return ctrl.Result{}, err
	}

	remaining := 0
	for _, child := range children {
		remaining++

		if !child.DeletionTimestamp.IsZero() {
			continue
		}

		if err := client.IgnoreNotFound(r.Delete(ctx, child)); err != nil {
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

	if err := removeFinalizer(ctx, r.Client, r.Recorder, log, rs, identityv1.ServiceAccountRoleReplicaSetFinalizer); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// applyReplicaSetCluster acts on a single rollout cluster using the planning-
// phase summary and cached child object. It performs no fresh Get calls; the
// child reference is whatever the cache observed at the start of reconcile.
//
//nolint:funlen,gocritic // hugeParam: planSummary is consumed by value to avoid aliasing the planning map; funlen tracks the apply state machine.
func (r *AWSServiceAccountRoleReplicaSetReconciler) applyReplicaSetCluster(ctx context.Context, log logr.Logger, rs *identityv1.AWSServiceAccountRoleReplicaSet, clusterName string, planSummary identityv1.AWSServiceAccountRoleClusterSummary, child *identityv1.AWSServiceAccountRole, template *replicaSetChildTemplate) identityv1.AWSServiceAccountRoleClusterSummary {
	if planSummary.Phase != identityv1.AWSServiceAccountRoleClusterPending {
		return planSummary
	}

	summary := planSummary
	summary.Reason = identityv1.ReasonChildrenPending
	summary.Message = messageWaitingForChildReadiness
	key := client.ObjectKey{Namespace: clusterName, Name: rs.Name}

	if child == nil {
		created := template.materialize(clusterName)
		if err := r.Create(ctx, created); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Cache lag: the child watch will requeue once the cache observes it.
				summary.Message = "AWSServiceAccountRole child exists but is not yet observed by the cache"

				return summary
			}
			summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
			summary.Reason = identityv1.ReasonChildApplyFailed
			summary.Message = fmt.Sprintf("create child role: %v", err)

			return summary
		}

		logChildApply(log, metrics.ControllerRoleReplicaSet, "AWSServiceAccountRole", key.String(), controllerutil.OperationResultCreated, nil)

		return summary
	}

	if !sameServiceAccountSubject(child.Spec.ServiceAccount, rs.Spec.Template.Spec.ServiceAccount) {
		if child.DeletionTimestamp.IsZero() {
			if err := client.IgnoreNotFound(r.Delete(ctx, child)); err != nil {
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

	if template.drifts(child) {
		updated := child.DeepCopy()
		template.apply(updated)
		if err := r.Patch(ctx, updated, client.MergeFromWithOptions(child, client.MergeFromWithOptimisticLock{})); err != nil {
			summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
			summary.Reason = identityv1.ReasonChildApplyFailed
			summary.Message = fmt.Sprintf("patch child role: %v", err)

			return summary
		}

		logChildApply(log, metrics.ControllerRoleReplicaSet, "AWSServiceAccountRole", key.String(), controllerutil.OperationResultUpdated, nil)
	}

	return summary
}

// pruneReplicaSetChildren deletes children whose namespace is no longer in the
// selected set. Children are taken as-is from the caller's pre-filtered list.
func (r *AWSServiceAccountRoleReplicaSetReconciler) pruneReplicaSetChildren(ctx context.Context, children []*identityv1.AWSServiceAccountRole, selected sets.Set[string]) int {
	pruned := 0
	for _, child := range children {
		if selected.Has(child.Namespace) {
			continue
		}

		if !child.DeletionTimestamp.IsZero() {
			pruned++

			continue
		}

		if err := client.IgnoreNotFound(r.Delete(ctx, child)); err != nil {
			logf.FromContext(ctx).Error(err, "delete stale AWSServiceAccountRole",
				"namespace", child.Namespace, "name", child.Name)

			continue
		}

		pruned++
	}

	return pruned
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) listOwnedReplicaSetChildren(ctx context.Context, rs *identityv1.AWSServiceAccountRoleReplicaSet) ([]*identityv1.AWSServiceAccountRole, error) {
	list := &identityv1.AWSServiceAccountRoleList{}
	if err := r.List(ctx, list, roleByReplicaSetUIDKey(string(rs.UID))); err != nil {
		return nil, fmt.Errorf("list AWSServiceAccountRoles by ReplicaSet UID %q: %w", rs.UID, err)
	}

	owned := make([]*identityv1.AWSServiceAccountRole, 0, len(list.Items))
	for i := range list.Items {
		child := &list.Items[i]
		if replicaSetOwnsChild(rs, child) {
			owned = append(owned, child)
		}
	}

	return owned, nil
}

type resolvedPlacement struct {
	placement     *clusterv1beta1.Placement
	clusterGroups clustersdkv1beta1.ClusterGroupsMap
}

type replicaSetPlacementResolution struct {
	clusters          sets.Set[string]
	byRef             map[string]resolvedPlacement
	placementStatuses []identityv1.AWSServiceAccountRolePlacementStatus
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) resolveReplicaSetPlacements(ctx context.Context, rs *identityv1.AWSServiceAccountRoleReplicaSet) (replicaSetPlacementResolution, error) {
	result := replicaSetPlacementResolution{
		clusters:          sets.New[string](),
		byRef:             map[string]resolvedPlacement{},
		placementStatuses: make([]identityv1.AWSServiceAccountRolePlacementStatus, 0, len(rs.Spec.PlacementRefs)),
	}
	previousConditions := map[string][]metav1.Condition{}
	for _, placement := range rs.Status.Placements {
		previousConditions[placement.Name] = placement.Conditions
	}

	var errs error
	for _, ref := range rs.Spec.PlacementRefs {
		placement, clusterGroups, err := r.resolveOCMPlacement(ctx, rs.Namespace, ref.Name)
		status := identityv1.AWSServiceAccountRolePlacementStatus{
			Name:       ref.Name,
			Conditions: slices.Clone(previousConditions[ref.Name]),
		}
		if err != nil {
			errs = errors.Join(errs, err)
			setCondition(&status.Conditions, rs.Generation, identityv1.ConditionPlacementResolved, metav1.ConditionFalse, identityv1.ReasonPlacementUnavailable, err.Error())
		} else {
			setCondition(&status.Conditions, rs.Generation, identityv1.ConditionPlacementResolved, metav1.ConditionTrue, identityv1.ReasonResolved, "OCM Placement and PlacementDecision objects resolved")
		}

		clusters := clusterGroups.GetClusters()
		result.byRef[ref.Name] = resolvedPlacement{placement: placement, clusterGroups: clusterGroups}
		status.SelectedClusterCount = countInt32(clusters.Len())
		result.placementStatuses = append(result.placementStatuses, status)
		for c := range clusters {
			result.clusters.Insert(c)
		}
	}

	return result, errs
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) resolveOCMPlacement(ctx context.Context, namespace, name string) (*clusterv1beta1.Placement, clustersdkv1beta1.ClusterGroupsMap, error) {
	placement := &clusterv1beta1.Placement{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, placement); err != nil {
		return nil, nil, fmt.Errorf("resolve OCM Placement %s/%s: %w", namespace, name, err)
	}

	tracker := clustersdkv1beta1.NewPlacementDecisionClustersTrackerWithGroups(
		placement,
		placementDecisionGetter{ctx: ctx, client: r.Client, placement: placement},
		nil,
	)
	if err := tracker.Refresh(); err != nil {
		return placement, nil, fmt.Errorf("resolve OCM PlacementDecision groups for %s/%s: %w", namespace, name, err)
	}

	return placement, tracker.ExistingClusterGroupsBesides(), nil
}

// placementDecisionGetter adapts ctrl-runtime's client.Client to the OCM
// PlacementDecisionGetter interface, which requires a List(selector, namespace)
// signature without context. The OCM SDK shape forces the context to live on
// the struct here.
type placementDecisionGetter struct {
	ctx       context.Context //nolint:containedctx // OCM PlacementDecisionGetter interface omits ctx; carry it on the struct.
	client    client.Client
	placement *clusterv1beta1.Placement
}

func (g placementDecisionGetter) List(selector labels.Selector, namespace string) ([]*clusterv1beta1.PlacementDecision, error) {
	decisions := &clusterv1beta1.PlacementDecisionList{}
	if err := g.client.List(g.ctx, decisions, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list OCM PlacementDecisions in namespace %s: %w", namespace, err)
	}

	out := make([]*clusterv1beta1.PlacementDecision, 0, len(decisions.Items))
	for i := range decisions.Items {
		if !placementOwnsDecision(g.placement, &decisions.Items[i]) {
			continue
		}
		out = append(out, &decisions.Items[i])
	}

	return out, nil
}

func placementOwnsDecision(placement *clusterv1beta1.Placement, decision *clusterv1beta1.PlacementDecision) bool {
	return metav1.IsControlledBy(decision, placement)
}

type replicaSetRolloutPlan struct {
	rolloutClusters         sets.Set[string]
	summaries               map[string]identityv1.AWSServiceAccountRoleClusterSummary
	statuses                map[string]clustersdkv1alpha1.ClusterRolloutStatus
	placementRollouts       map[string]*identityv1.AWSServiceAccountRoleRolloutSummary
	availableDecisionGroups map[string]string
	summary                 *identityv1.AWSServiceAccountRoleRolloutSummary
	requeueAfter            time.Duration
}

//nolint:funlen // OCM rollout planning is clearer when the SDK result assembly stays in one pass over placementRefs.
func (r *AWSServiceAccountRoleReplicaSetReconciler) planReplicaSetRollout(ctx context.Context, rs *identityv1.AWSServiceAccountRoleReplicaSet, resolution replicaSetPlacementResolution, childrenByNS map[string]*identityv1.AWSServiceAccountRole, template *replicaSetChildTemplate) (replicaSetRolloutPlan, error) {
	plan := replicaSetRolloutPlan{
		rolloutClusters:         sets.New[string](),
		summaries:               map[string]identityv1.AWSServiceAccountRoleClusterSummary{},
		statuses:                map[string]clustersdkv1alpha1.ClusterRolloutStatus{},
		placementRollouts:       map[string]*identityv1.AWSServiceAccountRoleRolloutSummary{},
		availableDecisionGroups: map[string]string{},
	}
	timeoutClusters := sets.New[string]()

	for _, ref := range rs.Spec.PlacementRefs {
		resolved := resolution.byRef[ref.Name]
		clusterGroups := resolved.clusterGroups
		placement := resolved.placement
		if len(clusterGroups) == 0 {
			plan.placementRollouts[ref.Name] = newReplicaSetRolloutSummary(
				0,
				sets.New[string](),
				sets.New[string](),
				map[string]clustersdkv1alpha1.ClusterRolloutStatus{},
				placementRolloutConditions(rs, ref.Name),
				rs.Generation,
			)
			plan.availableDecisionGroups[ref.Name] = availableDecisionGroupsMessage(placement, clusterGroups, nil, nil)
			continue
		}

		refStatuses := map[string]clustersdkv1alpha1.ClusterRolloutStatus{}
		existingStatuses := make([]clustersdkv1alpha1.ClusterRolloutStatus, 0, clusterGroups.GetClusters().Len())
		for _, clusterName := range sets.List(clusterGroups.GetClusters()) {
			summary, status, err := r.existingReplicaSetClusterSummary(ctx, rs, clusterName, childrenByNS[clusterName], template)
			if err != nil {
				return plan, err
			}
			plan.summaries[clusterName] = summary
			plan.statuses[clusterName] = status
			refStatuses[clusterName] = status
			existingStatuses = append(existingStatuses, status)
		}

		tracker := clustersdkv1beta1.NewPlacementDecisionClustersTrackerWithGroups(nil, nil, clusterGroups)
		rolloutHandler, err := clustersdkv1alpha1.NewRolloutHandler[struct{}](tracker, noopClusterRolloutStatus)
		if err != nil {
			return plan, err
		}
		_, rolloutResult, err := rolloutHandler.GetRolloutCluster(defaultRolloutStrategy(ref.RolloutStrategy), existingStatuses)
		if err != nil {
			return plan, fmt.Errorf("plan rollout for placement ref %q: %w", ref.Name, err)
		}

		refRolloutClusters := sets.New[string]()
		refTimeoutClusters := sets.New[string]()
		for _, status := range rolloutResult.ClustersToRollout {
			if status.ClusterName == "" {
				continue
			}
			refStatuses[status.ClusterName] = status
			plan.statuses[status.ClusterName] = status
			refRolloutClusters.Insert(status.ClusterName)
			plan.rolloutClusters.Insert(status.ClusterName)
		}
		for _, status := range rolloutResult.ClustersTimeOut {
			if status.ClusterName == "" {
				continue
			}
			refStatuses[status.ClusterName] = status
			plan.statuses[status.ClusterName] = status
			plan.summaries[status.ClusterName] = summaryFromRolloutStatus(rs, status)
			refTimeoutClusters.Insert(status.ClusterName)
			timeoutClusters.Insert(status.ClusterName)
		}

		plan.placementRollouts[ref.Name] = newReplicaSetRolloutSummary(
			countInt32(clusterGroups.GetClusters().Len()),
			refRolloutClusters,
			refTimeoutClusters,
			refStatuses,
			placementRolloutConditions(rs, ref.Name),
			rs.Generation,
		)
		plan.availableDecisionGroups[ref.Name] = availableDecisionGroupsMessage(placement, clusterGroups, refStatuses, refRolloutClusters)
		if rolloutResult.RecheckAfter != nil && *rolloutResult.RecheckAfter > 0 {
			if plan.requeueAfter == 0 {
				plan.requeueAfter = *rolloutResult.RecheckAfter
			} else {
				plan.requeueAfter = min(plan.requeueAfter, *rolloutResult.RecheckAfter)
			}
		}
	}

	plan.summary = newReplicaSetRolloutSummary(
		countInt32(resolution.clusters.Len()),
		plan.rolloutClusters,
		timeoutClusters,
		plan.statuses,
		rolloutConditions(rs.Status.Rollout),
		rs.Generation,
	)

	return plan, nil
}

func applyReplicaSetRolloutStatus(rs *identityv1.AWSServiceAccountRoleReplicaSet, rollout replicaSetRolloutPlan) {
	rs.Status.Rollout = rollout.summary
	for i := range rs.Status.Placements {
		name := rs.Status.Placements[i].Name
		rs.Status.Placements[i].Rollout = rollout.placementRollouts[name]
		rs.Status.Placements[i].AvailableDecisionGroups = rollout.availableDecisionGroups[name]
	}
	applyPlacementRolledOutCondition(&rs.Status, rollout.summary, rs.Generation)
}

func applyPlacementRolledOutCondition(status *identityv1.AWSServiceAccountRoleReplicaSetStatus, summary *identityv1.AWSServiceAccountRoleRolloutSummary, generation int64) {
	if summary == nil {
		return
	}
	progressing := meta.FindStatusCondition(summary.Conditions, identityv1.ConditionProgressing)
	if progressing == nil {
		return
	}
	if progressing.Status == metav1.ConditionFalse && progressing.Reason == identityv1.ReasonComplete {
		setCondition(&status.Conditions, generation, identityv1.ConditionPlacementRolledOut, metav1.ConditionTrue, identityv1.ReasonComplete, progressing.Message)
		return
	}
	setCondition(&status.Conditions, generation, identityv1.ConditionPlacementRolledOut, metav1.ConditionFalse, identityv1.ReasonProgressing, progressing.Message)
}

func placementRolloutConditions(rs *identityv1.AWSServiceAccountRoleReplicaSet, placementName string) []metav1.Condition {
	for _, placement := range rs.Status.Placements {
		if placement.Name == placementName {
			return rolloutConditions(placement.Rollout)
		}
	}

	return nil
}

func rolloutConditions(summary *identityv1.AWSServiceAccountRoleRolloutSummary) []metav1.Condition {
	if summary == nil {
		return nil
	}

	return slices.Clone(summary.Conditions)
}

func newReplicaSetRolloutSummary(
	total int32,
	updating sets.Set[string],
	timedOut sets.Set[string],
	statuses map[string]clustersdkv1alpha1.ClusterRolloutStatus,
	previousConditions []metav1.Condition,
	generation int64,
) *identityv1.AWSServiceAccountRoleRolloutSummary {
	summary := &identityv1.AWSServiceAccountRoleRolloutSummary{
		Total:      total,
		Updating:   countInt32(updating.Len()),
		TimedOut:   countInt32(timedOut.Len()),
		Conditions: previousConditions,
	}
	for clusterName, status := range statuses {
		if updating.Has(clusterName) || timedOut.Has(clusterName) {
			continue
		}
		switch status.Status {
		case clustersdkv1alpha1.Succeeded:
			summary.Succeeded++
		case clustersdkv1alpha1.Failed:
			summary.Failed++
		case clustersdkv1alpha1.TimeOut:
			summary.TimedOut++
		case clustersdkv1alpha1.ToApply, clustersdkv1alpha1.Progressing, clustersdkv1alpha1.Skip:
		}
	}

	if summary.Succeeded == summary.Total && summary.Failed == 0 && summary.TimedOut == 0 && summary.Updating == 0 {
		setCondition(&summary.Conditions, generation, identityv1.ConditionProgressing, metav1.ConditionFalse, identityv1.ReasonComplete,
			fmt.Sprintf("selected clusters %d. AWSServiceAccountRole children %d/%d completed with no errors, %d failed %d timed out.",
				summary.Total, summary.Succeeded, summary.Total, summary.Failed, summary.TimedOut))
		return summary
	}

	setCondition(&summary.Conditions, generation, identityv1.ConditionProgressing, metav1.ConditionTrue, identityv1.ReasonProgressing,
		fmt.Sprintf("selected clusters %d. AWSServiceAccountRole children %d/%d progressing, %d failed %d timed out.",
			summary.Total, summary.Updating+summary.Succeeded, summary.Total, summary.Failed, summary.TimedOut))

	return summary
}

func availableDecisionGroupsMessage(
	placement *clusterv1beta1.Placement,
	clusterGroups clustersdkv1beta1.ClusterGroupsMap,
	statuses map[string]clustersdkv1alpha1.ClusterRolloutStatus,
	applying sets.Set[string],
) string {
	totalGroups := len(clusterGroups)
	selectedClusters := clusterGroups.GetClusters()
	totalClusters := selectedClusters.Len()
	if placement != nil {
		if len(placement.Status.DecisionGroups) > 0 {
			totalGroups = len(placement.Status.DecisionGroups)
		}
		if placement.Status.NumberOfSelectedClusters > 0 {
			totalClusters = int(placement.Status.NumberOfSelectedClusters)
		}
	}

	appliedClusters := 0
	for clusterName, status := range statuses {
		if !selectedClusters.Has(clusterName) {
			continue
		}
		if applying.Has(clusterName) || status.Status != clustersdkv1alpha1.ToApply {
			appliedClusters++
		}
	}

	return fmt.Sprintf("%d (%d / %d clusters applied)", totalGroups, appliedClusters, totalClusters)
}

func noopClusterRolloutStatus(clusterName string, _ struct{}) (clustersdkv1alpha1.ClusterRolloutStatus, error) {
	return clustersdkv1alpha1.ClusterRolloutStatus{
		ClusterName: clusterName,
		Status:      clustersdkv1alpha1.ToApply,
	}, nil
}

func defaultRolloutStrategy(strategy clusterv1alpha1.RolloutStrategy) clusterv1alpha1.RolloutStrategy {
	if strategy.Type == "" {
		strategy.Type = clusterv1alpha1.All
	}

	return strategy
}

// existingReplicaSetClusterSummary returns the planning-phase view of one
// cluster: a user-facing summary plus the OCM rollout status the SDK consumes.
// child may be nil (no child observed); it is taken from the per-reconcile
// children map and not refetched.
//
//nolint:funlen // Read-only state machine mirrors apply branches; splitting them apart obscures the parallel.
func (r *AWSServiceAccountRoleReplicaSetReconciler) existingReplicaSetClusterSummary(ctx context.Context, rs *identityv1.AWSServiceAccountRoleReplicaSet, clusterName string, child *identityv1.AWSServiceAccountRole, template *replicaSetChildTemplate) (identityv1.AWSServiceAccountRoleClusterSummary, clustersdkv1alpha1.ClusterRolloutStatus, error) {
	summary := identityv1.AWSServiceAccountRoleClusterSummary{
		ClusterName: clusterName,
		Namespace:   clusterName,
		Name:        rs.Name,
		Phase:       identityv1.AWSServiceAccountRoleClusterPending,
		Reason:      identityv1.ReasonRolloutPending,
		Message:     messageWaitingForRolloutToApply,
	}
	status := clustersdkv1alpha1.ClusterRolloutStatus{
		ClusterName: clusterName,
		Status:      clustersdkv1alpha1.ToApply,
	}

	if err := r.Get(ctx, client.ObjectKey{Name: clusterName}, &corev1.Namespace{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return summary, status, fmt.Errorf("get cluster namespace %q: %w", clusterName, err)
		}
		summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
		summary.Reason = identityv1.ReasonClusterNamespaceMissing
		summary.Message = fmt.Sprintf("cluster namespace %q is required", clusterName)
		status.Status = clustersdkv1alpha1.Failed

		return summary, status, nil
	}

	if child == nil {
		// childrenByNS only contains owned children; an unowned existing object
		// is invisible here. Probe the cache for the conflict case.
		key := client.ObjectKey{Namespace: clusterName, Name: rs.Name}
		probe := &identityv1.AWSServiceAccountRole{}
		err := r.Get(ctx, key, probe)
		if err == nil && !replicaSetOwnsChild(rs, probe) {
			summary.Phase = identityv1.AWSServiceAccountRoleClusterConflict
			summary.Reason = identityv1.ReasonChildConflict
			summary.Message = fmt.Sprintf("AWSServiceAccountRole %s exists and is not owned by this ReplicaSet", key.String())
			status.Status = clustersdkv1alpha1.Failed

			return summary, status, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return summary, status, fmt.Errorf("get AWSServiceAccountRole %s: %w", key.String(), err)
		}

		return summary, status, nil
	}

	if !sameServiceAccountSubject(child.Spec.ServiceAccount, rs.Spec.Template.Spec.ServiceAccount) || template.drifts(child) {
		return summary, status, nil
	}

	ready := meta.FindStatusCondition(child.Status.Conditions, identityv1.ConditionReady)
	if ready != nil && !ready.LastTransitionTime.IsZero() {
		transition := ready.LastTransitionTime
		status.LastTransitionTime = &transition
	}
	if childReady(child) {
		summary.Phase = identityv1.AWSServiceAccountRoleClusterReady
		summary.Reason = identityv1.ReasonReady
		summary.Message = messageGeneratedChildReady
		status.Status = clustersdkv1alpha1.Succeeded

		return summary, status, nil
	}

	summary.Reason = identityv1.ReasonChildrenPending
	summary.Message = messageWaitingForChildReadiness
	status.Status = clustersdkv1alpha1.Progressing

	return summary, status, nil
}

func summaryFromRolloutStatus(rs *identityv1.AWSServiceAccountRoleReplicaSet, status clustersdkv1alpha1.ClusterRolloutStatus) identityv1.AWSServiceAccountRoleClusterSummary {
	summary := identityv1.AWSServiceAccountRoleClusterSummary{
		ClusterName: status.ClusterName,
		Namespace:   status.ClusterName,
		Name:        rs.Name,
		Phase:       identityv1.AWSServiceAccountRoleClusterPending,
		Reason:      identityv1.ReasonRolloutPending,
		Message:     messageWaitingForRolloutToApply,
	}

	switch status.Status {
	case clustersdkv1alpha1.Succeeded:
		summary.Phase = identityv1.AWSServiceAccountRoleClusterReady
		summary.Reason = identityv1.ReasonReady
		summary.Message = messageGeneratedChildReady
	case clustersdkv1alpha1.Failed:
		summary.Phase = identityv1.AWSServiceAccountRoleClusterFailed
		summary.Reason = identityv1.ReasonChildApplyFailed
		summary.Message = "generated AWSServiceAccountRole child failed before rollout could continue"
	case clustersdkv1alpha1.TimeOut:
		summary.Phase = identityv1.AWSServiceAccountRoleClusterTimedOut
		summary.Reason = identityv1.ReasonRolloutTimedOut
		summary.Message = "generated AWSServiceAccountRole child rollout timed out"
	case clustersdkv1alpha1.ToApply, clustersdkv1alpha1.Progressing, clustersdkv1alpha1.Skip:
	}

	return summary
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
			Watches(&clusterv1beta1.Placement{}, handler.EnqueueRequestsFromMapFunc(r.replicaSetsForOCMPlacement)).
			Watches(&clusterv1beta1.PlacementDecision{}, handler.EnqueueRequestsFromMapFunc(r.replicaSetsForOCMPlacementDecision))
	}

	if err := builder.Complete(r); err != nil {
		return fmt.Errorf("set up AWSServiceAccountRoleReplicaSet controller: %w", err)
	}

	return nil
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) replicaSetsForChildRole(ctx context.Context, obj client.Object) []reconcile.Request {
	if ownerRef := obj.GetAnnotations()[identityv1.AnnotationReplicaSetOwnerRef]; ownerRef != "" {
		if nn, err := namespacedNameFromString(ownerRef); err == nil {
			return []reconcile.Request{{NamespacedName: nn}}
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

	return requestsForReplicaSetsReferencingPlacement(ctx, log, r.Client, obj.GetNamespace(), obj.GetName())
}

func (r *AWSServiceAccountRoleReplicaSetReconciler) replicaSetsForOCMPlacementDecision(ctx context.Context, obj client.Object) []reconcile.Request {
	placementName := obj.GetLabels()[clusterv1beta1.PlacementLabel]
	if placementName == "" {
		return nil
	}

	log := watchMapLogger(ctx, metrics.ControllerRoleReplicaSet, "replicaSetsForOCMPlacementDecision", "PlacementDecision", obj)

	return requestsForReplicaSetsReferencingPlacement(ctx, log, r.Client, obj.GetNamespace(), placementName)
}

func requestsForReplicaSetsReferencingPlacement(ctx context.Context, log logr.Logger, c client.Reader, namespace, name string) []reconcile.Request {
	replicaSets := &identityv1.AWSServiceAccountRoleReplicaSetList{}
	if err := c.List(ctx, replicaSets, client.InNamespace(namespace)); err != nil {
		log.Error(err, "failed to list ReplicaSets for placement watch map")
		return nil
	}

	requests := make([]reconcile.Request, 0, len(replicaSets.Items))
	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		if replicaSetReferencesPlacement(rs, name) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rs)})
		}
	}

	return requests
}

func replicaSetReferencesPlacement(rs *identityv1.AWSServiceAccountRoleReplicaSet, name string) bool {
	return slices.ContainsFunc(rs.Spec.PlacementRefs, func(ref identityv1.PlacementRef) bool {
		return ref.Name == name
	})
}

// replicaSetChildTemplate is the desired child shape derived once per
// reconcile. The labels/annotations/spec do not depend on cluster name, so we
// avoid recomputing them in every per-cluster iteration.
type replicaSetChildTemplate struct {
	name        string
	labels      map[string]string
	annotations map[string]string
	spec        identityv1.AWSServiceAccountRoleSpec
}

func newReplicaSetChildTemplate(rs *identityv1.AWSServiceAccountRoleReplicaSet) *replicaSetChildTemplate {
	labels := map[string]string{}
	annotations := map[string]string{}

	if rs.Spec.Template.Metadata != nil {
		maps.Copy(labels, rs.Spec.Template.Metadata.Labels)
		maps.Copy(annotations, rs.Spec.Template.Metadata.Annotations)
	}

	labels[identityv1.LabelManagedBy] = identityv1.ManagedByValue
	labels[identityv1.LabelReplicaSetUID] = string(rs.UID)
	annotations[identityv1.AnnotationReplicaSetOwnerRef] = client.ObjectKeyFromObject(rs).String()

	return &replicaSetChildTemplate{
		name:        rs.Name,
		labels:      labels,
		annotations: annotations,
		spec:        *rs.Spec.Template.Spec.DeepCopy(),
	}
}

func (t *replicaSetChildTemplate) materialize(clusterName string) *identityv1.AWSServiceAccountRole {
	child := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.name,
			Namespace: clusterName,
		},
	}
	t.apply(child)

	return child
}

func (t *replicaSetChildTemplate) apply(child *identityv1.AWSServiceAccountRole) {
	child.Labels = maps.Clone(t.labels)
	child.Annotations = maps.Clone(t.annotations)
	child.Spec = *t.spec.DeepCopy()
}

func (t *replicaSetChildTemplate) drifts(child *identityv1.AWSServiceAccountRole) bool {
	return !apiequality.Semantic.DeepEqual(child.Labels, t.labels) ||
		!apiequality.Semantic.DeepEqual(child.Annotations, t.annotations) ||
		!apiequality.Semantic.DeepEqual(child.Spec, t.spec)
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
	if n > math.MaxInt32 {
		return math.MaxInt32
	}

	return int32(n) //nolint:gosec // bounded above by math.MaxInt32 before conversion
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
		if summary.Reason != identityv1.ReasonRolloutPending {
			rs.Status.AppliedClusterCount++
		}
		if summary.Reason == identityv1.ReasonImmutableChildDrift {
			rs.Status.StaleClusterCount++
		}
	case identityv1.AWSServiceAccountRoleClusterConflict:
		rs.Status.ConflictCount++
	case identityv1.AWSServiceAccountRoleClusterFailed, identityv1.AWSServiceAccountRoleClusterTimedOut:
		rs.Status.FailureCount++
	}
}

func appendReplicaSetSummary(rs *identityv1.AWSServiceAccountRoleReplicaSet, summary *identityv1.AWSServiceAccountRoleClusterSummary) {
	if len(rs.Status.Clusters) < maxReportedClusters {
		rs.Status.Clusters = append(rs.Status.Clusters, *summary)
	}

	if summary.Phase != identityv1.AWSServiceAccountRoleClusterFailed &&
		summary.Phase != identityv1.AWSServiceAccountRoleClusterConflict &&
		summary.Phase != identityv1.AWSServiceAccountRoleClusterTimedOut {
		return
	}

	if len(rs.Status.FailedClusters) >= maxReportedFailedClusters {
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
	childrenPendingMessage := fmt.Sprintf("%d of %d generated AWSServiceAccountRole children are ready", rs.Status.ReadyClusterCount, rs.Status.DesiredClusterCount)
	allReady := rs.Status.ReadyClusterCount == rs.Status.DesiredClusterCount &&
		rs.Status.FailureCount == 0 && rs.Status.ConflictCount == 0 && rs.Status.StaleClusterCount == 0

	switch {
	case rs.Status.FailureCount > 0:
		message := fmt.Sprintf("%d selected clusters failed to apply", rs.Status.FailureCount)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending, childrenPendingMessage)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed, message)
	case rs.Status.ConflictCount > 0:
		message := fmt.Sprintf("%d selected clusters have conflicting AWSServiceAccountRole children", rs.Status.ConflictCount)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonChildConflict, message)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending, childrenPendingMessage)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildConflict, message)
	case allReady:
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionTrue, identityv1.ReasonChildrenApplied, "generated AWSServiceAccountRole children are applied")
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionTrue, identityv1.ReasonChildrenReady, "generated AWSServiceAccountRole children are ready")
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "ReplicaSet is ready")
	default:
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionTrue, identityv1.ReasonChildrenApplied, "generated AWSServiceAccountRole children are applied")
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending, childrenPendingMessage)
		setCondition(&rs.Status.Conditions, rs.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending, childrenPendingMessage)
	}
}

func validateReplicaSetTemplateMetadata(rs *identityv1.AWSServiceAccountRoleReplicaSet) error {
	if rs.Spec.Template.Metadata == nil {
		return nil
	}

	if key := firstReservedKey(rs.Spec.Template.Metadata.Labels, reservedReplicaSetTemplateLabels); key != "" {
		return fmt.Errorf("spec.template.metadata.labels[%q] is reserved", key)
	}

	if key := firstReservedKey(rs.Spec.Template.Metadata.Annotations, reservedReplicaSetTemplateAnnotations); key != "" {
		return fmt.Errorf("spec.template.metadata.annotations[%q] is reserved", key)
	}

	return nil
}

func firstReservedKey(values map[string]string, reserved map[string]struct{}) string {
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if _, ok := reserved[key]; ok {
			return key
		}
	}

	return ""
}

var (
	reservedReplicaSetTemplateLabels = map[string]struct{}{
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

	reservedReplicaSetTemplateAnnotations = map[string]struct{}{
		identityv1.AnnotationReplicaSetOwnerRef: {},
	}
)

func hasMapping(mgr ctrl.Manager, gvk schema.GroupVersionKind) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)

	return err == nil
}
