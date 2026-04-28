package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestSelfHostedRoleEnqueueControllerSkipsUnmatchedServiceAccount(t *testing.T) {
	ch := make(chan event.TypedGenericEvent[*identityv1.AWSServiceAccountRole], 1)
	reconciler := &SelfHostedRoleEnqueueController{
		LocalClient: testConfigClient(t, roleForServiceAccount("app", "app")),
		RoleEvents:  ch,
	}

	if _, err := reconciler.Reconcile(context.Background(), serviceAccountRequest(testResolvedClusterName, "other")); err != nil {
		t.Fatal(err)
	}

	if len(ch) != 0 {
		t.Fatalf("expected no enqueue event, got %d", len(ch))
	}
}

func TestSelfHostedRoleEnqueueControllerEnqueuesMatchingRole(t *testing.T) {
	role := roleForServiceAccount("app", "app")
	ch := make(chan event.TypedGenericEvent[*identityv1.AWSServiceAccountRole], 1)
	reconciler := &SelfHostedRoleEnqueueController{
		LocalClient: testConfigClient(t, role),
		RoleEvents:  ch,
	}

	if _, err := reconciler.Reconcile(context.Background(), serviceAccountRequest(testResolvedClusterName, "app")); err != nil {
		t.Fatal(err)
	}

	select {
	case evt := <-ch:
		if evt.Object.Namespace != role.Namespace || evt.Object.Name != role.Name {
			t.Fatalf("unexpected enqueued role %s/%s", evt.Object.Namespace, evt.Object.Name)
		}
	default:
		t.Fatal("expected role enqueue event")
	}
}

func TestSelfHostedRoleEnqueueControllerSkipsInvalidClusterName(t *testing.T) {
	ch := make(chan event.TypedGenericEvent[*identityv1.AWSServiceAccountRole], 1)
	reconciler := &SelfHostedRoleEnqueueController{
		LocalClient: testConfigClient(t, roleForServiceAccount("app", "app")),
		RoleEvents:  ch,
	}

	if _, err := reconciler.Reconcile(context.Background(), serviceAccountRequest(testInventoryNamespace, "app")); err != nil {
		t.Fatal(err)
	}

	if len(ch) != 0 {
		t.Fatalf("expected no enqueue event, got %d", len(ch))
	}
}

func TestSelfHostedRoleEnqueueControllerReturnsIndexLookupError(t *testing.T) {
	listErr := errors.New("cache index unavailable")
	c := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*identityv1.AWSServiceAccountRoleList); ok {
					return listErr
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()
	reconciler := &SelfHostedRoleEnqueueController{
		LocalClient: c,
		RoleEvents:  make(chan event.TypedGenericEvent[*identityv1.AWSServiceAccountRole], 1),
	}

	if _, err := reconciler.Reconcile(context.Background(), serviceAccountRequest(testResolvedClusterName, "app")); !errors.Is(err, listErr) {
		t.Fatalf("expected index lookup error, got %v", err)
	}
}

func TestSelfHostedRoleEnqueueControllerRequeuesWhenChannelFull(t *testing.T) {
	ch := make(chan event.TypedGenericEvent[*identityv1.AWSServiceAccountRole], 1)
	ch <- event.TypedGenericEvent[*identityv1.AWSServiceAccountRole]{Object: roleForServiceAccount("already", "already")}

	reconciler := &SelfHostedRoleEnqueueController{
		LocalClient: testConfigClient(t, roleForServiceAccount("app", "app")),
		RoleEvents:  ch,
	}

	result, err := reconciler.Reconcile(context.Background(), serviceAccountRequest(testResolvedClusterName, "app"))
	if err != nil {
		t.Fatal(err)
	}

	if result.RequeueAfter != channelFullRequeue {
		t.Fatalf("expected RequeueAfter=%s, got %s", channelFullRequeue, result.RequeueAfter)
	}

	if len(ch) != 1 {
		t.Fatalf("expected full channel to keep existing event only, got %d events", len(ch))
	}
}

func TestAnnotatedServiceAccountDeletePredicateKeepsOnlyAnnotatedDeletes(t *testing.T) {
	predicate := annotatedServiceAccountDeletePredicate()
	annotated := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      "app",
		Namespace: "default",
		Annotations: map[string]string{
			selfHostedRoleARNAnnotation: testRoleARN,
		},
	}}
	unannotated := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}

	if !predicate.Delete(event.DeleteEvent{Object: annotated}) {
		t.Fatal("expected annotated ServiceAccount delete to be kept")
	}

	if predicate.Create(event.CreateEvent{Object: annotated}) {
		t.Fatal("expected annotated ServiceAccount create to be dropped")
	}

	if predicate.Update(event.UpdateEvent{ObjectOld: unannotated, ObjectNew: annotated}) {
		t.Fatal("expected annotated ServiceAccount update to be dropped")
	}

	if predicate.Generic(event.GenericEvent{Object: annotated}) {
		t.Fatal("expected annotated ServiceAccount generic event to be dropped")
	}

	if predicate.Delete(event.DeleteEvent{Object: unannotated}) {
		t.Fatal("expected unannotated ServiceAccount delete to be dropped")
	}
}

func serviceAccountRequest(clusterName, name string) mcreconcile.Request {
	return mcreconcile.Request{
		Request:     ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}},
		ClusterName: multicluster.ClusterName(clusterName),
	}
}

func roleForServiceAccount(roleName, saName string) *identityv1.AWSServiceAccountRole {
	return roleForServiceAccountInNamespace(roleName, testInventoryNamespace, saName)
}

func roleForServiceAccountInNamespace(roleName, namespace, saName string) *identityv1.AWSServiceAccountRole {
	return &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespace},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: saName},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
		},
	}
}
