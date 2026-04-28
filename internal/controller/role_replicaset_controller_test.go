//nolint:wsl_v5 // Tests group assertions by behavior; extra blank-line rules add noise here.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
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

// TestRoleReplicaSetDeleteCleansChildrenWithDriftedUIDLabel covers CR-02: cross-
// namespace ownership of AWSServiceAccountRole children is now established by
// the namespaced-name annotation AnnotationReplicaSetOwnerRef (stable across
// parent UID changes), not by LabelReplicaSetUID (mutable). A child whose UID
// label drifted (external mutation, or the RS was recreated with the same name
// but a new UID) must STILL be recognized as owned and cleaned up when the
// parent RS is deleted. Without the owner-ref annotation lookup, the index miss
// would orphan the child and leak the parent finalizer forever.
func TestRoleReplicaSetDeleteCleansChildrenWithDriftedUIDLabel(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	controllerutil.AddFinalizer(rs, identityv1.ServiceAccountRoleReplicaSetFinalizer)
	now := metav1.Now()
	rs.DeletionTimestamp = &now
	drifted := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testReplicaSetName,
			Namespace: "cluster-a",
			Labels: map[string]string{
				identityv1.LabelManagedBy:     identityv1.ManagedByValue,
				identityv1.LabelReplicaSetUID: "stale-uid-from-prior-generation",
			},
			Annotations: map[string]string{
				identityv1.AnnotationReplicaSetOwnerRef: testReplicaSetNamespace + "/" + testReplicaSetName,
			},
		},
		Spec: rs.Spec.Template.Spec,
	}
	c := testConfigClient(t, rs, testNamespace("cluster-a"), drifted)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	// First pass: drifted child must be recognized as owned (via annotation,
	// despite the foreign UID label) and issued for deletion. The reconciler
	// still requeues this pass because remaining > 0 was observed before the
	// delete fired; the parent finalizer is preserved until the child is gone.
	result, err := reconciler.reconcileReplicaSetDelete(ctx, ctrl.Log, rs)
	if err != nil {
		t.Fatalf("expected first delete reconcile to issue child delete, got result=%#v err=%v", result, err)
	}
	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue while child delete is in flight, got %#v", result)
	}

	if err := c.Get(ctx, client.ObjectKey{Namespace: "cluster-a", Name: testReplicaSetName}, &identityv1.AWSServiceAccountRole{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected drifted child to be deleted via owner-ref annotation lookup, got %v", err)
	}

	// Second pass: with the child gone, listOwnedReplicaSetChildren returns
	// empty and the reconciler must remove the finalizer to let the parent
	// finish deleting. This is the cleanup-completes leg of the regression.
	storedRS := getRoleReplicaSet(t, c, rs)
	result, err = reconciler.reconcileReplicaSetDelete(ctx, ctrl.Log, storedRS)
	if err != nil {
		t.Fatalf("expected second delete reconcile to complete cleanup, got result=%#v err=%v", result, err)
	}
	if !result.IsZero() {
		t.Fatalf("expected no requeue once owned child is gone, got %#v", result)
	}

	// Removing the last finalizer on a DeletionTimestamp'd object lets the
	// fake client finalize the parent — NotFound is the observable proof
	// that the finalizer was removed and cleanup completed. (The in-memory
	// `rs` still carries the finalizer slice because removeFinalizer mutates
	// only the in-cluster object via Patch.)
	err = c.Get(ctx, client.ObjectKeyFromObject(rs), &identityv1.AWSServiceAccountRoleReplicaSet{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected parent ReplicaSet to be fully reaped after finalizer removal, got %v", err)
	}
}

// TestReplicaSetOwnsChildOwnerRefAnnotationOverridesUIDLabel locks in the CR-02
// fix at the predicate boundary: replicaSetOwnsChild keys on the namespaced-name
// owner-ref annotation, not on LabelReplicaSetUID. The UID label is still
// stamped for forensics but must not gate ownership — drifted/missing values
// stay owned as long as managed-by + annotation match, and a wrong annotation
// drops ownership even when the UID label is present.
//
//nolint:funlen // table-driven cases kept inline; extracting them obscures the per-case label/annotation/want pairing.
func TestReplicaSetOwnsChildOwnerRefAnnotationOverridesUIDLabel(t *testing.T) {
	rs := testRoleReplicaSet()
	ownerRef := testReplicaSetNamespace + "/" + testReplicaSetName

	tests := []struct {
		name  string
		child *identityv1.AWSServiceAccountRole
		want  bool
	}{
		{
			name: "managed-by + correct annotation + drifted UID label",
			child: &identityv1.AWSServiceAccountRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testReplicaSetName,
					Namespace: "cluster-a",
					Labels: map[string]string{
						identityv1.LabelManagedBy:     identityv1.ManagedByValue,
						identityv1.LabelReplicaSetUID: "stale-uid-from-prior-generation",
					},
					Annotations: map[string]string{
						identityv1.AnnotationReplicaSetOwnerRef: ownerRef,
					},
				},
			},
			want: true,
		},
		{
			name: "managed-by + correct annotation + no UID label",
			child: &identityv1.AWSServiceAccountRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testReplicaSetName,
					Namespace: "cluster-a",
					Labels: map[string]string{
						identityv1.LabelManagedBy: identityv1.ManagedByValue,
					},
					Annotations: map[string]string{
						identityv1.AnnotationReplicaSetOwnerRef: ownerRef,
					},
				},
			},
			want: true,
		},
		{
			name: "managed-by + wrong annotation",
			child: &identityv1.AWSServiceAccountRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testReplicaSetName,
					Namespace: "cluster-a",
					Labels: map[string]string{
						identityv1.LabelManagedBy:     identityv1.ManagedByValue,
						identityv1.LabelReplicaSetUID: string(testReplicaSetUID),
					},
					Annotations: map[string]string{
						identityv1.AnnotationReplicaSetOwnerRef: "other-ns/other-rs",
					},
				},
			},
			want: false,
		},
		{
			name: "no managed-by label",
			child: &identityv1.AWSServiceAccountRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testReplicaSetName,
					Namespace: "cluster-a",
					Labels: map[string]string{
						identityv1.LabelReplicaSetUID: string(testReplicaSetUID),
					},
					Annotations: map[string]string{
						identityv1.AnnotationReplicaSetOwnerRef: ownerRef,
					},
				},
			},
			want: false,
		},
		{
			name: "managed-by but no annotation",
			child: &identityv1.AWSServiceAccountRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testReplicaSetName,
					Namespace: "cluster-a",
					Labels: map[string]string{
						identityv1.LabelManagedBy:     identityv1.ManagedByValue,
						identityv1.LabelReplicaSetUID: string(testReplicaSetUID),
					},
				},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := replicaSetOwnsChild(rs, tc.child); got != tc.want {
				t.Fatalf("replicaSetOwnsChild = %v, want %v (child labels=%#v annotations=%#v)", got, tc.want, tc.child.Labels, tc.child.Annotations)
			}
		})
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
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
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

// When a child Create returns a transient apiserver error, the reconciler must
// surface that error to controller-runtime so its rate-limited backoff fires —
// observing it only via status would force retries to wait for a watch event or
// the default 10h informer resync. The status patch must still happen first so
// FailureCount / Phase=Failed / Reason=ChildApplyFailed are observable, and
// Result must be zero alongside the error to keep the rate limiter authoritative.
func TestRoleReplicaSetSurfacesChildApplyErrorForRateLimitedBackoff(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	createErr := apierrors.NewInternalError(errors.New("apiserver throttled"))
	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(
			rs,
			testNamespace(testReplicaSetNamespace),
			testNamespace("cluster-a"),
			testOCMPlacement(),
			testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
		).
		WithStatusSubresource(rs).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*identityv1.AWSServiceAccountRole); ok {
					return createErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	result, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs)
	if err == nil {
		t.Fatalf("expected child apply failure to surface as error, got nil")
	}
	if !errors.Is(err, createErr) {
		t.Fatalf("expected wrapped createErr to surface via errors.Is, got %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero Result alongside error so controller-runtime applies its rate-limited backoff, got %#v", result)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.FailureCount != 1 {
		t.Fatalf("expected FailureCount=1 (status patched before error bubbles), got %#v", stored.Status)
	}
	assertClusterSummaryReason(t, stored.Status.Clusters, "cluster-a", identityv1.ReasonChildApplyFailed)
	foundFailedPhase := false
	for _, cluster := range stored.Status.Clusters {
		if cluster.ClusterName == "cluster-a" {
			if cluster.Phase != identityv1.AWSServiceAccountRoleClusterFailed {
				t.Fatalf("expected cluster-a Phase=Failed, got %#v", cluster)
			}
			foundFailedPhase = true
		}
	}
	if !foundFailedPhase {
		t.Fatalf("expected cluster-a summary, got %#v", stored.Status.Clusters)
	}
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionAWSServiceAccountRolesApplied, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
}

// Mirrors TestRoleReplicaSetSurfacesChildApplyErrorForRateLimitedBackoff but for the prune branch (see `pruneReplicaSetChildren` in role_replicaset_controller.go).
func TestRoleReplicaSetSurfacesStalePruneErrorForRateLimitedBackoff(t *testing.T) {
	ctx := context.Background()
	rs := testRoleReplicaSet()
	stale := testOwnedChild(rs, "cluster-stale")
	deleteErr := errors.New("simulated stale prune failure")
	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(
			rs,
			testNamespace(testReplicaSetNamespace),
			testNamespace("cluster-a"),
			testNamespace("cluster-stale"),
			testOCMPlacement(),
			testOCMPlacementDecision("22222222-2222-2222-2222-222222222222", "cluster-a"),
			stale,
		).
		WithStatusSubresource(rs).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*identityv1.AWSServiceAccountRole); ok && obj.GetNamespace() == "cluster-stale" {
					return deleteErr
				}

				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{Client: c}

	result, err := reconciler.reconcileReplicaSetNormal(ctx, ctrl.Log, rs)
	if err == nil {
		t.Fatalf("expected stale prune failure to surface as error, got nil")
	}
	if !errors.Is(err, deleteErr) {
		t.Fatalf("expected wrapped deleteErr to surface via errors.Is, got %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero Result alongside error so controller-runtime applies its rate-limited backoff, got %#v", result)
	}

	// Failed stale child must remain so the retry can re-attempt the delete.
	storedStale := getRole(t, c, "cluster-stale")
	if !storedStale.DeletionTimestamp.IsZero() {
		t.Fatalf("expected stale child to remain after transient prune failure, got DeletionTimestamp=%v", storedStale.DeletionTimestamp)
	}

	stored := getRoleReplicaSet(t, c, rs)
	if stored.Status.StaleClusterCount != 0 {
		t.Fatalf("expected StaleClusterCount=0 since the only stale prune failed, got %#v", stored.Status)
	}
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

// testChildRoleWithAnnotation returns a minimal AWSServiceAccountRole suitable
// for driving replicaSetsForChildRole. Pass the empty string to omit the
// AnnotationReplicaSetOwnerRef stamp entirely so we can exercise the
// missing-annotation early-return.
func testChildRoleWithAnnotation(ownerRef string) *identityv1.AWSServiceAccountRole {
	child := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testReplicaSetName,
			Namespace: "cluster-a",
		},
	}
	if ownerRef != "" {
		child.Annotations = map[string]string{
			identityv1.AnnotationReplicaSetOwnerRef: ownerRef,
		}
	}

	return child
}

// listFailingClient wraps a fake client with an interceptor that fails the test
// if List is invoked. replicaSetsForChildRole used to fall back to an unbounded
// LIST when the owner-ref annotation was missing or malformed; the early-return
// fast path must never trigger that fallback.
func listFailingClient(t *testing.T) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				t.Fatalf("unexpected List call: %T", list)

				return nil
			},
		}).
		Build()
}

func TestReplicaSetsForChildRoleEnqueuesParentFromAnnotation(t *testing.T) {
	// Build a fake client with NO AWSServiceAccountRoleReplicaSet objects.
	// If replicaSetsForChildRole tried to satisfy the request via a LIST it
	// would observe an empty result; the fact that we still get a request back
	// proves the function derives the parent solely from the child's
	// annotation, never from a cache LIST.
	c := testConfigClient(t)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
		Client:   c,
		Recorder: recorder,
	}

	child := testChildRoleWithAnnotation(testReplicaSetNamespace + "/" + testReplicaSetName)

	got := reconciler.replicaSetsForChildRole(context.Background(), child)
	if len(got) != 1 {
		t.Fatalf("expected exactly one reconcile request, got %#v", got)
	}
	want := types.NamespacedName{Namespace: testReplicaSetNamespace, Name: testReplicaSetName}
	if got[0].NamespacedName != want {
		t.Fatalf("expected request %#v, got %#v", want, got[0].NamespacedName)
	}
}

func TestReplicaSetsForChildRoleReturnsNilWhenAnnotationMissing(t *testing.T) {
	// listFailingClient.List will t.Fatalf if invoked. The fast path must
	// short-circuit before any LIST happens.
	c := listFailingClient(t)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
		Client:   c,
		Recorder: recorder,
	}

	child := testChildRoleWithAnnotation("")

	got := reconciler.replicaSetsForChildRole(context.Background(), child)
	if len(got) != 0 {
		t.Fatalf("expected no reconcile requests for child without owner-ref annotation, got %#v", got)
	}
}

func TestReplicaSetsForChildRoleReturnsNilWhenAnnotationMalformed(t *testing.T) {
	// namespacedNameFromString requires a "namespace/name" form; a value with
	// no slash is malformed and must drop without falling back to LIST.
	c := listFailingClient(t)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
		Client:   c,
		Recorder: recorder,
	}

	child := testChildRoleWithAnnotation("this-is-not-a-namespaced-name-because-it-has-no-slash")

	got := reconciler.replicaSetsForChildRole(context.Background(), child)
	if len(got) != 0 {
		t.Fatalf("expected no reconcile requests for child with malformed owner-ref annotation, got %#v", got)
	}
}

func TestIndexAWSServiceAccountRoleReplicaSetByPlacementRefReturnsEveryRefName(t *testing.T) {
	rs := &identityv1.AWSServiceAccountRoleReplicaSet{
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{
				{Name: "prod"},
				{Name: "staging"},
				{Name: ""}, // empty name must be skipped
			},
		},
	}

	got := IndexAWSServiceAccountRoleReplicaSetByPlacementRef(rs)
	want := []string{"prod", "staging"}

	if len(got) != len(want) {
		t.Fatalf("expected %d index entries, got %d (%#v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected entry %d %q, got %q", i, want[i], got[i])
		}
	}
}

func TestIndexAWSServiceAccountRoleReplicaSetByPlacementRefIgnoresOtherTypes(t *testing.T) {
	if got := IndexAWSServiceAccountRoleReplicaSetByPlacementRef(&identityv1.AWSServiceAccountRole{}); got != nil {
		t.Fatalf("expected nil for wrong object type, got %#v", got)
	}
}

func TestReplicaSetsForOCMPlacementEnqueuesOnlyReferencingReplicaSets(t *testing.T) {
	// Three ReplicaSets in the same namespace; only "rs-prod" and "rs-multi"
	// reference Placement "prod". The OCM Placement watch handler must use
	// the IndexReplicaSetByPlacementRef field index, returning only those
	// two ReplicaSets — never falling back to a namespace-wide LIST that
	// would also return "rs-staging".
	rsProd := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-prod",
			UID:       types.UID("rs-prod-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "prod"}},
		},
	}
	rsStaging := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-staging",
			UID:       types.UID("rs-staging-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "staging"}},
		},
	}
	rsMulti := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-multi",
			UID:       types.UID("rs-multi-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "prod"}, {Name: "staging"}},
		},
	}

	c := testConfigClient(t, rsProd, rsStaging, rsMulti)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
		Client:   c,
		Recorder: &capturingEventRecorder{},
	}

	placement := &clusterv1beta1.Placement{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "prod",
		},
	}

	got := reconciler.replicaSetsForOCMPlacement(context.Background(), placement)

	wantNames := map[string]struct{}{"rs-prod": {}, "rs-multi": {}}
	if len(got) != len(wantNames) {
		t.Fatalf("expected %d reconcile requests, got %d (%#v)", len(wantNames), len(got), got)
	}
	for _, req := range got {
		if req.Namespace != testReplicaSetNamespace {
			t.Fatalf("unexpected namespace %q in request %#v", req.Namespace, req)
		}
		if _, ok := wantNames[req.Name]; !ok {
			t.Fatalf("unexpected ReplicaSet %q enqueued for placement watch", req.Name)
		}
	}
}

func TestReplicaSetsForOCMPlacementDecisionEnqueuesViaPlacementLabel(t *testing.T) {
	// PlacementDecision events carry the parent Placement name via the
	// clusterv1beta1.PlacementLabel. The handler must resolve that label and
	// route the event through the same indexed lookup; a decision whose
	// label is missing must enqueue nothing.
	rsProd := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-prod",
			UID:       types.UID("rs-prod-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "prod"}},
		},
	}

	c := testConfigClient(t, rsProd)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
		Client:   c,
		Recorder: &capturingEventRecorder{},
	}

	decision := &clusterv1beta1.PlacementDecision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "prod-decision-1",
			Labels:    map[string]string{clusterv1beta1.PlacementLabel: "prod"},
		},
	}

	got := reconciler.replicaSetsForOCMPlacementDecision(context.Background(), decision)
	if len(got) != 1 {
		t.Fatalf("expected exactly one reconcile request, got %#v", got)
	}
	if got[0].Namespace != testReplicaSetNamespace || got[0].Name != "rs-prod" {
		t.Fatalf("unexpected reconcile request %#v", got[0].NamespacedName)
	}

	decisionWithoutLabel := &clusterv1beta1.PlacementDecision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "prod-decision-2",
		},
	}
	if got := reconciler.replicaSetsForOCMPlacementDecision(context.Background(), decisionWithoutLabel); len(got) != 0 {
		t.Fatalf("expected no reconcile requests for unlabeled PlacementDecision, got %#v", got)
	}
}

// TestRequestsForReplicaSetsReferencingPlacementFallsBackWhenIndexedListFails
// covers CR-04: when the indexed LIST (MatchingFields on
// replicaSetByPlacementRefKey) fails — e.g., because the field indexer was
// not registered against the live cache — the watch map function must NOT
// silently drop the event. Instead it falls back to a namespace-scoped
// non-indexed LIST and filters in-memory via replicaSetReferencesPlacement.
// The fallback stays namespace-bound (no cluster-wide widen).
func TestRequestsForReplicaSetsReferencingPlacementFallsBackWhenIndexedListFails(t *testing.T) {
	rsProd := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-prod",
			UID:       types.UID("rs-prod-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "prod"}},
		},
	}
	rsStaging := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-staging",
			UID:       types.UID("rs-staging-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "staging"}},
		},
	}

	indexedErr := errors.New("simulated indexed list failure")
	c := newReplicaSetIndexedListFailingClient(t, indexedErr, rsProd, rsStaging)

	got := requestsForReplicaSetsReferencingPlacement(context.Background(), logr.Discard(), c, testReplicaSetNamespace, "prod", "replicaSetsForOCMPlacement", "Placement")
	if got == nil {
		t.Fatalf("expected non-nil requests slice from fallback path, got nil")
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one fallback request, got %#v", got)
	}
	if got[0].Namespace != testReplicaSetNamespace || got[0].Name != "rs-prod" {
		t.Fatalf("unexpected fallback request %#v", got[0].NamespacedName)
	}
}

// TestRequestsForReplicaSetsReferencingPlacementReturnsNilWhenBothListsFail
// covers the last-resort branch of CR-04: when BOTH the indexed LIST and the
// namespace-scoped fallback LIST fail, the watch map function must return nil
// (drop the event) instead of panicking. The error is logged but cannot be
// returned because EnqueueRequestsFromMapFunc map functions have no error
// channel.
func TestRequestsForReplicaSetsReferencingPlacementReturnsNilWhenBothListsFail(t *testing.T) {
	rsProd := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-prod",
			UID:       types.UID("rs-prod-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "prod"}},
		},
	}

	listErr := errors.New("simulated total list failure")
	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(rsProd).
		WithIndex(&identityv1.AWSServiceAccountRoleReplicaSet{}, IndexReplicaSetByPlacementRef, IndexAWSServiceAccountRoleReplicaSetByPlacementRef).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*identityv1.AWSServiceAccountRoleReplicaSetList); ok {
					return listErr
				}

				return nil
			},
		}).
		Build()

	got := requestsForReplicaSetsReferencingPlacement(context.Background(), logr.Discard(), c, testReplicaSetNamespace, "prod", "replicaSetsForOCMPlacement", "Placement")
	if got != nil {
		t.Fatalf("expected nil requests when both indexed and fallback lists fail, got %#v", got)
	}
}

// TestRequestsForReplicaSetsReferencingPlacementRecordsMetricOnIndexedListFailure
// covers NS-WATCH-MAP-OBS-02: when the primary indexed LIST fails but the
// namespace-scoped fallback succeeds, the watch map function MUST increment
// awio_watch_map_list_errors_total exactly once with stable labels
// (controller=role_replicaset, map_func=replicaSetsForOCMPlacement,
// kind=Placement). Without this metric, indexer registration drift would
// silently drop Placement watch events with no on-call signal.
func TestRequestsForReplicaSetsReferencingPlacementRecordsMetricOnIndexedListFailure(t *testing.T) {
	rsProd := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-prod",
			UID:       types.UID("rs-prod-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "prod"}},
		},
	}

	indexedErr := errors.New("simulated indexed list failure")
	c := newReplicaSetIndexedListFailingClient(t, indexedErr, rsProd)

	before := watchMapListErrorCount(t, "replicaSetsForOCMPlacement", "Placement")

	got := requestsForReplicaSetsReferencingPlacement(context.Background(), logr.Discard(), c, testReplicaSetNamespace, "prod", "replicaSetsForOCMPlacement", "Placement")
	if len(got) != 1 || got[0].Name != rsProd.Name {
		t.Fatalf("expected fallback to return rs-prod, got %#v", got)
	}

	after := watchMapListErrorCount(t, "replicaSetsForOCMPlacement", "Placement")
	if delta := after - before; delta != 1 {
		t.Fatalf("expected watchMapListErrorsTotal delta 1 for primary indexed LIST failure, got %v (before=%v after=%v)", delta, before, after)
	}
}

// TestRequestsForReplicaSetsReferencingPlacementRecordsMetricOnBothListFailures
// covers NS-WATCH-MAP-OBS-02: when BOTH the indexed LIST and the namespace-
// scoped fallback LIST fail, the watch map function MUST increment
// awio_watch_map_list_errors_total exactly twice (once per LIST call) so that
// on-call sees the full failure rate and can distinguish single-path drift
// from total apiserver outage. Labels stay stable
// (controller=role_replicaset, map_func=replicaSetsForOCMPlacementDecision,
// kind=PlacementDecision) — covers the PlacementDecision caller path.
func TestRequestsForReplicaSetsReferencingPlacementRecordsMetricOnBothListFailures(t *testing.T) {
	rsProd := &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      "rs-prod",
			UID:       types.UID("rs-prod-uid"),
		},
		Spec: identityv1.AWSServiceAccountRoleReplicaSetSpec{
			PlacementRefs: []identityv1.PlacementRef{{Name: "prod"}},
		},
	}

	listErr := errors.New("simulated total list failure")
	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(rsProd).
		WithIndex(&identityv1.AWSServiceAccountRoleReplicaSet{}, IndexReplicaSetByPlacementRef, IndexAWSServiceAccountRoleReplicaSetByPlacementRef).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*identityv1.AWSServiceAccountRoleReplicaSetList); ok {
					return listErr
				}

				return nil
			},
		}).
		Build()

	before := watchMapListErrorCount(t, "replicaSetsForOCMPlacementDecision", "PlacementDecision")

	got := requestsForReplicaSetsReferencingPlacement(context.Background(), logr.Discard(), c, testReplicaSetNamespace, "prod", "replicaSetsForOCMPlacementDecision", "PlacementDecision")
	if got != nil {
		t.Fatalf("expected nil requests when both indexed and fallback lists fail, got %#v", got)
	}

	after := watchMapListErrorCount(t, "replicaSetsForOCMPlacementDecision", "PlacementDecision")
	if delta := after - before; delta != 2 {
		t.Fatalf("expected watchMapListErrorsTotal delta 2 (primary +1, fallback +1) when both LISTs fail, got %v (before=%v after=%v)", delta, before, after)
	}
}

// watchMapListErrorCount reads the current value of
// awio_watch_map_list_errors_total{controller=role_replicaset,map_func,kind}
// from the shared controller-runtime registry. The controller label is fixed
// because every caller in this test file targets the role_replicaset
// controller. Returns 0 when the series has not been recorded yet —
// counters with zero observations are absent from Gather().
func watchMapListErrorCount(t *testing.T, mapFunc, kind string) float64 {
	t.Helper()

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != "awio_watch_map_list_errors_total" {
			continue
		}

		for _, m := range family.GetMetric() {
			if labelValue(m.GetLabel(), "controller") != metrics.ControllerRoleReplicaSet {
				continue
			}
			if labelValue(m.GetLabel(), "map_func") != mapFunc {
				continue
			}
			if labelValue(m.GetLabel(), "kind") != kind {
				continue
			}

			return m.GetCounter().GetValue()
		}
	}

	return 0
}

// newReplicaSetIndexedListFailingClient returns a fake client whose
// AWSServiceAccountRoleReplicaSet List calls fail with indexedErr ONLY when
// the caller passes a MatchingFields option (i.e., FieldSelector is set on
// the resolved ListOptions). Namespace-only Lists succeed. This selectively
// simulates indexer registration drift on the indexed code path while
// keeping the namespace-scoped fallback path working.
func newReplicaSetIndexedListFailingClient(t *testing.T, indexedErr error, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(objs...).
		WithIndex(&identityv1.AWSServiceAccountRoleReplicaSet{}, IndexReplicaSetByPlacementRef, IndexAWSServiceAccountRoleReplicaSetByPlacementRef).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*identityv1.AWSServiceAccountRoleReplicaSetList); ok {
					resolved := &client.ListOptions{}
					for _, opt := range opts {
						opt.ApplyToList(resolved)
					}

					if resolved.FieldSelector != nil {
						return indexedErr
					}
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()
}

// testReplicaSetWithClusterStatus returns an AWSServiceAccountRoleReplicaSet
// whose status.clusters is initialized to summaries. This lets CR-01 tests
// drive replicaSetWaitingOnNamespace / replicaSetsForNamespace from
// per-cluster Reason values without going through the full reconcile path.
func testReplicaSetWithClusterStatus(name string, summaries ...identityv1.AWSServiceAccountRoleClusterSummary) *identityv1.AWSServiceAccountRoleReplicaSet {
	return &identityv1.AWSServiceAccountRoleReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testReplicaSetNamespace,
			Name:      name,
			UID:       types.UID(name + "-uid"),
		},
		Status: identityv1.AWSServiceAccountRoleReplicaSetStatus{
			Clusters: summaries,
		},
	}
}

// TestReplicaSetWaitingOnNamespace covers CR-01's predicate helper. The
// table-driven cases assert the helper only returns true when a status entry
// records BOTH the exact cluster (namespace) name AND
// ReasonClusterNamespaceMissing — a single mismatch on either field must
// drop, otherwise unrelated Namespace events would re-enqueue ReplicaSets
// fleet-wide.
//
//nolint:funlen // table-driven cases kept inline; extracting them obscures the per-case Reason/ClusterName/want pairing.
func TestReplicaSetWaitingOnNamespace(t *testing.T) {
	tests := []struct {
		name      string
		summaries []identityv1.AWSServiceAccountRoleClusterSummary
		nsName    string
		want      bool
	}{
		{
			name:      "empty status clusters",
			summaries: nil,
			nsName:    "cluster-a",
			want:      false,
		},
		{
			name: "matching name and ReasonClusterNamespaceMissing",
			summaries: []identityv1.AWSServiceAccountRoleClusterSummary{
				{ClusterName: "cluster-a", Phase: identityv1.AWSServiceAccountRoleClusterFailed, Reason: identityv1.ReasonClusterNamespaceMissing},
			},
			nsName: "cluster-a",
			want:   true,
		},
		{
			name: "matching name but Reason=Ready",
			summaries: []identityv1.AWSServiceAccountRoleClusterSummary{
				{ClusterName: "cluster-a", Phase: identityv1.AWSServiceAccountRoleClusterReady, Reason: identityv1.ReasonReady},
			},
			nsName: "cluster-a",
			want:   false,
		},
		{
			name: "matching name but Reason=RolloutPending",
			summaries: []identityv1.AWSServiceAccountRoleClusterSummary{
				{ClusterName: "cluster-a", Phase: identityv1.AWSServiceAccountRoleClusterPending, Reason: identityv1.ReasonRolloutPending},
			},
			nsName: "cluster-a",
			want:   false,
		},
		{
			name: "ReasonClusterNamespaceMissing but different cluster name",
			summaries: []identityv1.AWSServiceAccountRoleClusterSummary{
				{ClusterName: "cluster-b", Phase: identityv1.AWSServiceAccountRoleClusterFailed, Reason: identityv1.ReasonClusterNamespaceMissing},
			},
			nsName: "cluster-a",
			want:   false,
		},
		{
			name: "multiple entries, only one matches",
			summaries: []identityv1.AWSServiceAccountRoleClusterSummary{
				{ClusterName: "cluster-b", Phase: identityv1.AWSServiceAccountRoleClusterReady, Reason: identityv1.ReasonReady},
				{ClusterName: "cluster-a", Phase: identityv1.AWSServiceAccountRoleClusterFailed, Reason: identityv1.ReasonClusterNamespaceMissing},
				{ClusterName: "cluster-c", Phase: identityv1.AWSServiceAccountRoleClusterPending, Reason: identityv1.ReasonRolloutPending},
			},
			nsName: "cluster-a",
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := testReplicaSetWithClusterStatus("rs", tc.summaries...)
			if got := replicaSetWaitingOnNamespace(rs, tc.nsName); got != tc.want {
				t.Fatalf("replicaSetWaitingOnNamespace(%q) = %v, want %v", tc.nsName, got, tc.want)
			}
		})
	}
}

// TestReplicaSetsForNamespace covers CR-01's Namespace -> ReplicaSet fan-out.
// Cases assert the map only emits Requests for ReplicaSets whose status
// records the appearing namespace as missing; ReplicaSets parked on other
// namespaces, or already past the missing-namespace state, must contribute
// zero Requests.
//
//nolint:funlen // table-driven cases kept inline; extracting them obscures the per-case fixtures/want pairing.
func TestReplicaSetsForNamespace(t *testing.T) {
	missingClusterA := identityv1.AWSServiceAccountRoleClusterSummary{
		ClusterName: "cluster-a",
		Phase:       identityv1.AWSServiceAccountRoleClusterFailed,
		Reason:      identityv1.ReasonClusterNamespaceMissing,
	}
	readyClusterA := identityv1.AWSServiceAccountRoleClusterSummary{
		ClusterName: "cluster-a",
		Phase:       identityv1.AWSServiceAccountRoleClusterReady,
		Reason:      identityv1.ReasonReady,
	}

	tests := []struct {
		name      string
		replicas  []*identityv1.AWSServiceAccountRoleReplicaSet
		nsName    string
		wantNames map[string]struct{}
	}{
		{
			name:      "no ReplicaSets in cache",
			replicas:  nil,
			nsName:    "cluster-a",
			wantNames: map[string]struct{}{},
		},
		{
			name: "ReplicaSet parked on cluster-a, namespace event cluster-b",
			replicas: []*identityv1.AWSServiceAccountRoleReplicaSet{
				testReplicaSetWithClusterStatus("rs-parked", missingClusterA),
			},
			nsName:    "cluster-b",
			wantNames: map[string]struct{}{},
		},
		{
			name: "ReplicaSet parked on cluster-a, namespace event cluster-a",
			replicas: []*identityv1.AWSServiceAccountRoleReplicaSet{
				testReplicaSetWithClusterStatus("rs-parked", missingClusterA),
			},
			nsName:    "cluster-a",
			wantNames: map[string]struct{}{"rs-parked": {}},
		},
		{
			name: "two ReplicaSets parked on cluster-a",
			replicas: []*identityv1.AWSServiceAccountRoleReplicaSet{
				testReplicaSetWithClusterStatus("rs-a", missingClusterA),
				testReplicaSetWithClusterStatus("rs-b", missingClusterA),
			},
			nsName:    "cluster-a",
			wantNames: map[string]struct{}{"rs-a": {}, "rs-b": {}},
		},
		{
			name: "ReplicaSet Ready on cluster-a, namespace event cluster-a",
			replicas: []*identityv1.AWSServiceAccountRoleReplicaSet{
				testReplicaSetWithClusterStatus("rs-ready", readyClusterA),
			},
			nsName:    "cluster-a",
			wantNames: map[string]struct{}{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tc.replicas))
			for _, rs := range tc.replicas {
				objs = append(objs, rs)
			}
			c := testConfigClient(t, objs...)
			reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
				Client:   c,
				Recorder: &capturingEventRecorder{},
			}

			got := reconciler.replicaSetsForNamespace(context.Background(), testNamespace(tc.nsName))

			if len(got) != len(tc.wantNames) {
				t.Fatalf("expected %d reconcile requests, got %d (%#v)", len(tc.wantNames), len(got), got)
			}
			for _, req := range got {
				if req.Namespace != testReplicaSetNamespace {
					t.Fatalf("unexpected namespace %q in request %#v", req.Namespace, req)
				}
				if _, ok := tc.wantNames[req.Name]; !ok {
					t.Fatalf("unexpected ReplicaSet %q enqueued for namespace event %q", req.Name, tc.nsName)
				}
			}
		})
	}
}

// TestReplicaSetsForNamespaceIgnoresEmptyName guards the early-return branch:
// a Namespace object without a name should never trigger a LIST. We assert
// that explicitly by routing the call through a List-failing fake client.
func TestReplicaSetsForNamespaceIgnoresEmptyName(t *testing.T) {
	c := listFailingClient(t)
	reconciler := &AWSServiceAccountRoleReplicaSetReconciler{
		Client:   c,
		Recorder: &capturingEventRecorder{},
	}

	if got := reconciler.replicaSetsForNamespace(context.Background(), &corev1.Namespace{}); len(got) != 0 {
		t.Fatalf("expected no reconcile requests for empty-name Namespace, got %#v", got)
	}
}

// TestNamespaceAppearedPredicate asserts the CR-01 watch predicate only keeps
// Namespace CreateEvents. Update / Delete / Generic events for namespaces
// must be dropped — once a namespace exists the ReplicaSet reconciler relies
// on per-cluster Role children to drive subsequent transitions, so admitting
// those events would cause fleet-wide hot reconciliation on unrelated
// Namespace status/label churn.
func TestNamespaceAppearedPredicate(t *testing.T) {
	pred := namespaceAppearedPredicate()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"}}

	if !pred.Create(event.CreateEvent{Object: ns}) {
		t.Fatal("expected CreateEvent to be kept")
	}

	if pred.Update(event.UpdateEvent{ObjectOld: ns, ObjectNew: ns}) {
		t.Fatal("expected UpdateEvent to be dropped")
	}

	if pred.Delete(event.DeleteEvent{Object: ns}) {
		t.Fatal("expected DeleteEvent to be dropped")
	}

	if pred.Generic(event.GenericEvent{Object: ns}) {
		t.Fatal("expected GenericEvent to be dropped")
	}
}
