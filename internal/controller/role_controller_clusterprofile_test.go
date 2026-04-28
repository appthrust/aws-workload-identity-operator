package controller

import (
	"context"
	"slices"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
)

// TestRolesForClusterProfileOCMFallbackEnqueuesNamespace verifies that an
// OCM-fallback ClusterProfile (carrying open-cluster-management.io/cluster-name)
// enqueues every AWSServiceAccountRole living in the workload namespace named
// by that label, regardless of the ClusterProfile's own namespace.
func TestRolesForClusterProfileOCMFallbackEnqueuesNamespace(t *testing.T) {
	roleA := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-a", Name: "role-a"},
	}
	roleB := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-a", Name: "role-b"},
	}
	// Sentinel in a different namespace must NOT be enqueued.
	roleOther := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-b", Name: "role-other"},
	}

	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(roleA, roleB, roleOther).
		Build()

	r := &AWSServiceAccountRoleReconciler{Client: c}

	profile := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "awio-system",
			Name:      "downstream-handle",
			Labels: map[string]string{
				inventory.LabelOCMClusterName: "ws-a",
			},
		},
	}

	requests := r.rolesForClusterProfile(context.Background(), profile)
	got := sortedRequestKeys(requests)
	want := []string{"ws-a/role-a", "ws-a/role-b"}

	if !slices.Equal(got, want) {
		t.Fatalf("expected requests %v, got %v", want, got)
	}
}

// TestRolesForClusterProfileUnresolvableReturnsNil verifies that a
// ClusterProfile with no OCM label and Namespace != Name resolves to an empty
// namespace and therefore enqueues nothing.
func TestRolesForClusterProfileUnresolvableReturnsNil(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-a", Name: "role-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role).
		Build()

	r := &AWSServiceAccountRoleReconciler{Client: c}

	profile := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "awio-system", Name: "orphan"},
	}

	requests := r.rolesForClusterProfile(context.Background(), profile)
	if requests != nil {
		t.Fatalf("expected nil requests for unresolvable ClusterProfile, got %#v", requests)
	}
}

// TestRolesForClusterProfileWrongTypeReturnsNil guards against a silent
// regression if the watch source ever delivers a non-ClusterProfile object:
// the cast must fail closed and not enqueue requests.
func TestRolesForClusterProfileWrongTypeReturnsNil(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-a", Name: "role-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role).
		Build()

	r := &AWSServiceAccountRoleReconciler{Client: c}

	var obj client.Object = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-a", Name: "not-a-cluster-profile"},
	}

	requests := r.rolesForClusterProfile(context.Background(), obj)
	if requests != nil {
		t.Fatalf("expected nil requests for non-ClusterProfile object, got %#v", requests)
	}
}

func sortedRequestKeys(requests []reconcile.Request) []string {
	out := make([]string, 0, len(requests))
	for _, req := range requests {
		out = append(out, req.Namespace+"/"+req.Name)
	}

	sort.Strings(out)

	return out
}
