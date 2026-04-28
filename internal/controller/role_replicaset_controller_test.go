//nolint:wsl_v5 // Tests group assertions by behavior; extra blank-line rules add noise here.
package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

const (
	testReplicaSetNamespace = "fleet"
	testReplicaSetName      = "app"
	testReplicaSetUID       = types.UID("11111111-1111-1111-1111-111111111111")
)

func TestRoleReplicaSetReconcileAddsFinalizerWithoutExplicitRequeue(t *testing.T) {
	rs := testRoleReplicaSet()
	localClient := testConfigClient(t, rs)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
		Client:   localClient,
		Recorder: recorder,
	}

	assertFinalizerAddedOnFirstReconcile(t, localClient, reconciler, rs, &identityv1.AWSServiceAccountRoleReplicaSet{}, identityv1.ServiceAccountRoleReplicaSetFinalizer, recorder)
}

func TestRoleReplicaSetCreatesChildrenForOCMPlacement(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testNamespace("cluster-a"),
		testNamespace("cluster-b"),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-b", "cluster-a"),
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	for _, clusterName := range []string{"cluster-a", "cluster-b"} {
		child := getRole(t, c, clusterName)
		if child.Labels[identityv1.LabelManagedBy] != identityv1.ManagedByValue ||
			child.Labels[identityv1.LabelReplicaSetUID] != string(testReplicaSetUID) {
			t.Fatalf("expected child ownership labels, got %#v", child.Labels)
		}
		if child.Annotations[identityv1.AnnotationReplicaSetOwnerRef] != testReplicaSetNamespace+"/"+testReplicaSetName {
			t.Fatalf("expected owner annotation, got %#v", child.Annotations)
		}
		if !sameServiceAccountSubject(child.Spec.ServiceAccount, rs.Spec.Template.Spec.ServiceAccount) ||
			len(child.Spec.PolicyARNs) != 1 ||
			child.Spec.PolicyARNs[0] != "arn:aws:iam::123456789012:policy/AppPolicy" {
			t.Fatalf("unexpected child spec: %#v", child.Spec)
		}
		if child.Labels["app.kubernetes.io/name"] != "app" || child.Annotations["example.com/team"] != "platform" {
			t.Fatalf("expected template metadata to be copied, labels=%#v annotations=%#v", child.Labels, child.Annotations)
		}
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.SelectedClusterCount != 2 ||
		stored.Status.AppliedClusterCount != 2 ||
		stored.Status.ReadyClusterCount != 0 {
		t.Fatalf("unexpected status counts: %#v", stored.Status)
	}
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionPlacementResolved, metav1.ConditionTrue, identityv1.ReasonResolved)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionTrue, identityv1.ReasonChildrenApplied)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending)
}

func TestRoleReplicaSetHonorsProgressiveRollout(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	rs.Spec.PlacementRefs[0].RolloutStrategy = clusterv1alpha1.RolloutStrategy{
		Type: clusterv1alpha1.Progressive,
		Progressive: &clusterv1alpha1.RolloutProgressive{
			MaxConcurrency: intstr.FromInt(1),
		},
	}
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testNamespace("cluster-a"),
		testNamespace("cluster-b"),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a", "cluster-b"),
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	if err := c.Get(ctx, client.ObjectKey{Namespace: "cluster-a", Name: testReplicaSetName}, &identityv1.AWSServiceAccountRole{}); err != nil {
		t.Fatalf("expected first rollout cluster child, got %v", err)
	}
	err := c.Get(ctx, client.ObjectKey{Namespace: "cluster-b", Name: testReplicaSetName}, &identityv1.AWSServiceAccountRole{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected second cluster to wait for rollout, got %v", err)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.SelectedClusterCount != 2 ||
		stored.Status.AppliedClusterCount != 1 ||
		stored.Status.ReadyClusterCount != 0 {
		t.Fatalf("unexpected progressive status counts: %#v", stored.Status)
	}
	if stored.Status.Rollout == nil ||
		stored.Status.Rollout.Total != 2 ||
		stored.Status.Rollout.Updating != 1 ||
		stored.Status.Rollout.Succeeded != 0 {
		t.Fatalf("unexpected rollout summary: %#v", stored.Status.Rollout)
	}
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionPlacementRolledOut, metav1.ConditionFalse, identityv1.ReasonProgressing)
	assertClusterSummaryReason(t, stored.Status.Clusters, "cluster-b", identityv1.ReasonRolloutPending)
}

func TestRoleReplicaSetPropagatesChildReady(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	child := testOwnedChild(rs, "cluster-a")
	child.Generation = 7
	setCondition(&child.Status.Conditions, child.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testNamespace("cluster-a"),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
		child,
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.ReadyClusterCount != 1 {
		t.Fatalf("expected ready count 1, got %#v", stored.Status)
	}
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionTrue, identityv1.ReasonChildrenReady)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady)
}

func TestRoleReplicaSetDoesNotAdoptForeignChild(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	foreign := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testReplicaSetName,
			Namespace: "cluster-a",
			Labels:    map[string]string{"example.com/owner": "user"},
		},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "other", Name: "foreign"},
			PolicyARNs:     []string{"arn:aws:iam::123456789012:policy/Foreign"},
		},
	}
	before := foreign.DeepCopy()
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testNamespace("cluster-a"),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
		foreign,
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	after := getRole(t, c, "cluster-a")
	if !apiequality.Semantic.DeepEqual(before.Labels, after.Labels) ||
		!apiequality.Semantic.DeepEqual(before.Annotations, after.Annotations) ||
		!apiequality.Semantic.DeepEqual(before.Spec, after.Spec) {
		t.Fatalf("foreign child was modified\nbefore=%#v\nafter=%#v", before, after)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.ConflictCount != 1 {
		t.Fatalf("expected conflict count 1, got %#v", stored.Status)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonChildConflict)
}

func TestRoleReplicaSetPrunesOwnedStaleChildren(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	stale := testOwnedChild(rs, "cluster-b")
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testNamespace("cluster-a"),
		testNamespace("cluster-b"),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
		stale,
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	err := c.Get(ctx, client.ObjectKey{Namespace: "cluster-b", Name: testReplicaSetName}, &identityv1.AWSServiceAccountRole{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected stale child to be deleted, got %v", err)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.StaleClusterCount != 1 {
		t.Fatalf("expected stale count 1, got %#v", stored.Status)
	}
}

func TestRoleReplicaSetPruneIgnoresUIDLabelWithoutManagedBy(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	mislabelled := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testReplicaSetName,
			Namespace: "cluster-b",
			Labels: map[string]string{
				identityv1.LabelReplicaSetUID: string(testReplicaSetUID),
			},
		},
		Spec: rs.Spec.Template.Spec,
	}
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testNamespace("cluster-a"),
		testNamespace("cluster-b"),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
		mislabelled,
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	stored := getRole(t, c, "cluster-b")
	if stored.DeletionTimestamp != nil {
		t.Fatal("mislabelled child without managed-by label was incorrectly pruned")
	}
}

func TestRoleReplicaSetDoesNotPruneSelectedChildWhenClusterApplyFails(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	child := testOwnedChild(rs, "cluster-a")
	controllerutil.AddFinalizer(child, identityv1.ServiceAccountRoleFinalizer)
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
		child,
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	storedChild := getRole(t, c, "cluster-a")
	if !storedChild.DeletionTimestamp.IsZero() {
		t.Fatal("selected child was incorrectly pruned after cluster apply failure")
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.FailureCount != 1 || stored.Status.StaleClusterCount != 0 {
		t.Fatalf("expected one apply failure without stale prune, got %#v", stored.Status)
	}
}

func TestRoleReplicaSetDeleteWaitsForOwnedChildren(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	controllerutil.AddFinalizer(rs, identityv1.ServiceAccountRoleReplicaSetFinalizer)
	now := metav1.Now()
	rs.DeletionTimestamp = &now
	child := testOwnedChild(rs, "cluster-a")
	controllerutil.AddFinalizer(child, identityv1.ServiceAccountRoleFinalizer)
	c := testConfigClient(t, rs, child)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	result, err := reconciler.reconcileReplicaSetDelete(ctx, ctrl.Log, rs)
	if err != nil {
		t.Fatalf("expected delete reconcile to block cleanly, got result=%#v err=%v", result, err)
	}
	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue, got %#v", result)
	}

	storedRS := getRoleReplicaSet(t, c, rs)
	if !controllerutil.ContainsFinalizer(storedRS, identityv1.ServiceAccountRoleReplicaSetFinalizer) {
		t.Fatalf("expected parent finalizer to remain, got %#v", storedRS.Finalizers)
	}
	assertCondition(t, storedRS.Status.Conditions, identityv1.ConditionCleanupBlocked, metav1.ConditionTrue, identityv1.ReasonDeletionBlocked)

	storedChild := getRole(t, c, "cluster-a")
	if storedChild.DeletionTimestamp.IsZero() {
		t.Fatal("expected child deletion timestamp to be set")
	}
}

func TestRoleReplicaSetDeleteIgnoresUIDLabelWithoutManagedBy(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	controllerutil.AddFinalizer(rs, identityv1.ServiceAccountRoleReplicaSetFinalizer)
	now := metav1.Now()
	rs.DeletionTimestamp = &now
	mislabelled := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testReplicaSetName,
			Namespace: "cluster-a",
			Labels: map[string]string{
				identityv1.LabelReplicaSetUID: string(testReplicaSetUID),
			},
		},
		Spec: rs.Spec.Template.Spec,
	}
	c := testConfigClient(t, rs, mislabelled)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	result, err := reconciler.reconcileReplicaSetDelete(ctx, ctrl.Log, rs)
	if err != nil {
		t.Fatalf("expected delete reconcile to ignore mislabelled child, got result=%#v err=%v", result, err)
	}
	if !result.IsZero() {
		t.Fatalf("expected no requeue when no owned children remain, got %#v", result)
	}

	storedChild := getRole(t, c, "cluster-a")
	if storedChild.DeletionTimestamp != nil {
		t.Fatal("mislabelled child without managed-by label was incorrectly deleted")
	}
}

func TestRoleReplicaSetIgnoresUnownedOCMPlacementDecision(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	placement := testOCMPlacement()
	unownedDecision := testOCMPlacementDecision("33333333-3333-3333-3333-333333333333", "cluster-a")
	c := testConfigClient(t, rs, testNamespace(testReplicaSetNamespace), testNamespace("cluster-a"), placement, unownedDecision)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.SelectedClusterCount != 0 || stored.Status.AppliedClusterCount != 0 {
		t.Fatalf("expected unowned decision to be ignored, got %#v", stored.Status)
	}
}

func TestRoleReplicaSetRejectsReservedTemplateMetadata(t *testing.T) {
	rs := testRoleReplicaSet()
	rs.Spec.Template.Metadata.Labels[identityv1.LabelReplicaSetUID] = "foreign"

	if _, err := (RoleReplicaSetValidator{}).ValidateCreate(context.Background(), rs); err == nil {
		t.Fatal("expected reserved label to be rejected")
	}
}

func TestRoleReplicaSetPlacementResolveErrorPreservesAccumulatedStatus(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	rs.Generation = 5
	rs.Spec.PlacementRefs = []identityv1.PlacementRef{{
		Name: "missing",
	}}
	rs.Status.SelectedClusterCount = 2
	rs.Status.DesiredClusterCount = 2
	rs.Status.AppliedClusterCount = 2
	rs.Status.ReadyClusterCount = 2
	rs.Status.Clusters = []identityv1.AWSServiceAccountRoleClusterSummary{{
		ClusterName: "cluster-a",
		Namespace:   "cluster-a",
		Name:        rs.Name,
		Phase:       identityv1.AWSServiceAccountRoleClusterReady,
		Reason:      identityv1.ReasonReady,
		Message:     "generated AWSServiceAccountRole child is ready",
	}}
	setCondition(&rs.Status.Conditions, rs.Generation-1, identityv1.ConditionPlacementResolved, metav1.ConditionTrue, identityv1.ReasonResolved, "previously resolved")
	setCondition(&rs.Status.Conditions, rs.Generation-1, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionTrue, identityv1.ReasonChildrenApplied, "previously applied")
	setCondition(&rs.Status.Conditions, rs.Generation-1, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionTrue, identityv1.ReasonChildrenReady, "previously ready")
	setCondition(&rs.Status.Conditions, rs.Generation-1, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "previously ready")

	c := testConfigClient(t, rs, testNamespace(testReplicaSetNamespace))
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	result, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs)
	if err != nil {
		t.Fatalf("expected reconcile to surface the resolve error via status only, got %v", err)
	}
	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue, got %#v", result)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.ObservedGeneration != rs.Generation {
		t.Fatalf("expected ObservedGeneration=%d, got %d", rs.Generation, stored.Status.ObservedGeneration)
	}
	if stored.Status.SelectedClusterCount != 2 || stored.Status.ReadyClusterCount != 2 || len(stored.Status.Clusters) != 1 {
		t.Fatalf("expected accumulated counts/Clusters to be preserved across transient placement-resolve error, got %#v", stored.Status)
	}
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionPlacementResolved, metav1.ConditionFalse, identityv1.ReasonPlacementUnavailable)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonPlacementUnavailable)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionTrue, identityv1.ReasonChildrenApplied)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionTrue, identityv1.ReasonChildrenReady)
}

// When placement resolution fails AND the status patch fails, the reconciler
// must surface the patch error so controller-runtime applies its rate-limited
// backoff. Returning a non-zero RequeueAfter alongside an error would short-
// circuit the rate limiter and busy-loop on a misbehaving apiserver.
func TestRoleReplicaSetPlacementResolveStatusPatchErrorReturnsError(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	rs.Spec.PlacementRefs = []identityv1.PlacementRef{{
		Name: "missing",
	}}
	patchErr := errors.New("simulated status patch failure")
	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(rs, testNamespace(testReplicaSetNamespace)).
		WithStatusSubresource(rs).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetUID, IndexAWSServiceAccountRoleByReplicaSetUID).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if subResourceName == "status" {
					if _, ok := obj.(*identityv1.AWSServiceAccountRoleReplicaSet); ok {
						return patchErr
					}
				}

				return c.Status().Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	result, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs)
	if !errors.Is(err, patchErr) {
		t.Fatalf("expected status patch failure to surface, got %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero Result so controller-runtime applies its rate-limited backoff, got %#v", result)
	}
}

func TestRoleReplicaSetDowngradesReadyWhenChildBecomesNotReady(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	rs.Generation = 4
	setCondition(&rs.Status.Conditions, rs.Generation-1, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "ReplicaSet is ready")
	setCondition(&rs.Status.Conditions, rs.Generation-1, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionTrue, identityv1.ReasonChildrenReady, "generated AWSServiceAccountRole children are ready")

	notReadyChild := testOwnedChild(rs, "cluster-a")
	c := testConfigClient(t,
		rs,
		testNamespace(testReplicaSetNamespace),
		testNamespace("cluster-a"),
		testOCMPlacement(),
		testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
		notReadyChild,
	)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	if _, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs); err != nil {
		t.Fatal(err)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.ReadyClusterCount != 0 {
		t.Fatalf("expected ready count 0, got %#v", stored.Status)
	}
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionAWSServiceAccountRolesReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildrenPending)
}

func testRoleReplicaSet() *identityv1.AWSServiceAccountRoleReplicaSet {
	return &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testReplicaSetName,
			Namespace: testReplicaSetNamespace,
			UID:       testReplicaSetUID,
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{
				Name: "prod",
			}},
			Template: identityv1.AWSServiceAccountRoleTemplate{
				Metadata: &identityv1.TemplateMetadata{
					Labels:      map[string]string{"app.kubernetes.io/name": "app"},
					Annotations: map[string]string{"example.com/team": "platform"},
				},
				Spec: identityv1.AWSServiceAccountRoleSpec{
					ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "app", Name: "workload"},
					PolicyARNs:     []string{"arn:aws:iam::123456789012:policy/AppPolicy"},
				},
			},
		},
	}
}

func testOwnedChild(rs *identityv1.AWSServiceAccountRoleReplicaSet, clusterName string) *identityv1.AWSServiceAccountRole {
	child := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name,
			Namespace: clusterName,
			UID:       types.UID(clusterName + "-uid"),
		},
	}
	newReplicaSetChildTemplate(rs).apply(child)

	return child
}

func testNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func testOCMPlacement() *clusterv1beta1.Placement {
	return &clusterv1beta1.Placement{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "prod",
			UID:       "22222222-2222-2222-2222-222222222222",
		},
	}
}

func testOCMPlacementDecision(placementUID types.UID, clusterNames ...string) *clusterv1beta1.PlacementDecision {
	decisions := make([]clusterv1beta1.ClusterDecision, 0, len(clusterNames))
	for _, clusterName := range clusterNames {
		decisions = append(decisions, clusterv1beta1.ClusterDecision{ClusterName: clusterName, Reason: "selected"})
	}

	controller := true

	return &clusterv1beta1.PlacementDecision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "prod-decision-1",
			Labels: map[string]string{
				clusterv1beta1.PlacementLabel:          "prod",
				clusterv1beta1.DecisionGroupIndexLabel: "0",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: ocmClusterGroupVersion.String(),
				Kind:       "Placement",
				Name:       "prod",
				UID:        placementUID,
				Controller: &controller,
			}},
		},
		Status: clusterv1beta1.PlacementDecisionStatus{Decisions: decisions},
	}
}

func getRole(t *testing.T, c client.Client, namespace string) *identityv1.AWSServiceAccountRole {
	t.Helper()

	role := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: testReplicaSetName}, role); err != nil {
		t.Fatal(err)
	}

	return role
}

func getRoleReplicaSet(t *testing.T, c client.Client, rs *identityv1.AWSServiceAccountRoleReplicaSet) *identityv1.AWSServiceAccountRoleReplicaSet {
	t.Helper()

	stored := &identityv1.AWSServiceAccountRoleReplicaSet{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rs), stored); err != nil {
		t.Fatal(err)
	}

	return stored
}

func assertClusterSummaryReason(t *testing.T, clusters []identityv1.AWSServiceAccountRoleClusterSummary, clusterName, reason string) {
	t.Helper()

	for _, cluster := range clusters {
		if cluster.ClusterName != clusterName {
			continue
		}
		if cluster.Reason != reason {
			t.Fatalf("expected cluster %q reason %q, got %#v", clusterName, reason, cluster)
		}

		return
	}

	t.Fatalf("expected cluster %q summary in %#v", clusterName, clusters)
}
