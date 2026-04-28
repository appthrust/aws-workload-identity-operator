package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

// TestConfigForDeletedRoleEnqueuesNamespaceConfig pins the regression that
// AWSServiceAccountRole deletion events enqueue the singleton
// AWSWorkloadIdentityConfig in the role's own namespace, so reconcileDelete
// wakes promptly on the last child removal instead of waiting on the 30s
// deletion safety-net requeue.
func TestConfigForDeletedRoleEnqueuesNamespaceConfig(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: testInventoryNamespace,
		},
	}

	got := configForDeletedRole(context.Background(), role)

	want := []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: testInventoryNamespace,
			Name:      identityv1.DefaultName,
		},
	}}

	if !reconcileRequestsEqual(got, want) {
		t.Fatalf("configForDeletedRole(role) = %#v, want %#v", got, want)
	}
}

// TestConfigForDeletedRoleUsesRoleNamespace asserts the enqueued request
// targets the deleted role's own namespace (one Default config per workload
// namespace), not a fixed namespace. A bug here would silently fail to wake
// the correct config on multi-cluster setups.
func TestConfigForDeletedRoleUsesRoleNamespace(t *testing.T) {
	const otherNamespace = "wlc-b"

	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other",
			Namespace: otherNamespace,
		},
	}

	got := configForDeletedRole(context.Background(), role)

	want := []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: otherNamespace,
			Name:      identityv1.DefaultName,
		},
	}}

	if !reconcileRequestsEqual(got, want) {
		t.Fatalf("configForDeletedRole(role in %q) = %#v, want %#v", otherNamespace, got, want)
	}
}

// TestConfigForDeletedRoleIgnoresNonRoleObject asserts the map function is
// closed against accidental wiring to a different watched kind: only
// AWSServiceAccountRole objects produce requests, so a stray non-role event
// (here a Secret) returns nil rather than enqueueing the namespace config.
func TestConfigForDeletedRoleIgnoresNonRoleObject(t *testing.T) {
	notARole := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated",
			Namespace: testInventoryNamespace,
		},
	}

	if got := configForDeletedRole(context.Background(), notARole); got != nil {
		t.Fatalf("configForDeletedRole(non-role) = %#v, want nil", got)
	}
}

// TestRoleDeletedForConfigPredicateKeepsOnlyDelete asserts the predicate
// keeps Delete events and drops Create / Update / Generic, so the
// AWSWorkloadIdentityConfig reconciler wakes promptly on the last child
// role disappearing without paying the cost of every other role lifecycle
// transition (those cannot relax the "roles remain" gate that reconcileDelete
// guards).
func TestRoleDeletedForConfigPredicateKeepsOnlyDelete(t *testing.T) {
	pred := roleDeletedForConfigPredicate(metrics.ControllerConfig)

	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: testInventoryNamespace,
		},
	}

	if !pred.Delete(event.DeleteEvent{Object: role}) {
		t.Fatal("expected Delete event to be kept so reconcileDelete wakes promptly on last-role removal")
	}

	if pred.Create(event.CreateEvent{Object: role}) {
		t.Fatal("expected Create event to be dropped; new roles cannot relax the reconcileDelete child-roles gate")
	}

	if pred.Update(event.UpdateEvent{ObjectOld: role.DeepCopy(), ObjectNew: role.DeepCopy()}) {
		t.Fatal("expected Update event to be dropped; existing-role mutations cannot wake the parent config")
	}

	if pred.Generic(event.GenericEvent{Object: role}) {
		t.Fatal("expected Generic event to be dropped; no upstream produces generic role events for this watch")
	}
}

func reconcileRequestsEqual(a, b []reconcile.Request) bool {
	return slices.Equal(a, b)
}
