package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestPatchRemoteServiceAccountAnnotations(t *testing.T) {
	c := fakeClient(t, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "controller",
			Namespace: "kube-system",
			Annotations: map[string]string{
				"existing.example/key": testPreservedValue,
			},
		},
	})

	op, err := patchRemoteServiceAccountAnnotations(context.Background(), c, identityv1.ServiceAccountSubject{
		Namespace: "kube-system",
		Name:      "controller",
	}, "arn:aws:iam::123456789012:role/controller")
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultUpdated {
		t.Fatalf("expected update operation, got %q", op)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "kube-system"}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	if sa.Annotations["existing.example/key"] != testPreservedValue {
		t.Fatalf("expected existing annotation to be preserved: %#v", sa.Annotations)
	}

	if sa.Annotations[selfHostedRoleARNAnnotation] != "arn:aws:iam::123456789012:role/controller" ||
		sa.Annotations[selfHostedAudienceAnnotation] != "sts.amazonaws.com" ||
		sa.Annotations[selfHostedRegionalSTSAnnotation] != "true" ||
		sa.Annotations[selfHostedTokenExpirationAnnotation] != "86400" {
		t.Fatalf("unexpected annotations: %#v", sa.Annotations)
	}
}

func TestPatchRemoteServiceAccountAnnotationsMissingServiceAccount(t *testing.T) {
	c := fakeClient(t)

	_, err := patchRemoteServiceAccountAnnotations(context.Background(), c, identityv1.ServiceAccountSubject{
		Namespace: "missing",
		Name:      "app",
	}, "arn:aws:iam::123456789012:role/app")
	if err == nil || !strings.Contains(err.Error(), "remote ServiceAccount missing/app") {
		t.Fatalf("expected missing ServiceAccount error, got %v", err)
	}
}

func TestRemoveRemoteServiceAccountAnnotations(t *testing.T) {
	c := fakeClient(t, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: "default",
			Annotations: map[string]string{
				"existing.example/key":              testPreservedValue,
				selfHostedRoleARNAnnotation:         "arn:aws:iam::123456789012:role/app",
				selfHostedAudienceAnnotation:        "sts.amazonaws.com",
				selfHostedRegionalSTSAnnotation:     "true",
				selfHostedTokenExpirationAnnotation: "86400",
			},
		},
	})

	op, err := removeRemoteServiceAccountAnnotations(context.Background(), c, identityv1.ServiceAccountSubject{
		Namespace: "default",
		Name:      "app",
	}, "arn:aws:iam::123456789012:role/app")
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultUpdated {
		t.Fatalf("expected update operation, got %q", op)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	if sa.Annotations["existing.example/key"] != testPreservedValue {
		t.Fatalf("expected existing annotation to be preserved: %#v", sa.Annotations)
	}

	for _, key := range selfHostedServiceAccountAnnotationKeys() {
		if _, ok := sa.Annotations[key]; ok {
			t.Fatalf("expected %q to be removed: %#v", key, sa.Annotations)
		}
	}
}

func TestAnnotationRepairPredicateKeepsManagedServiceAccounts(t *testing.T) {
	predicate := annotationRepairPredicate()
	annotated := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      "app",
		Namespace: "default",
		Annotations: map[string]string{
			selfHostedRoleARNAnnotation: "arn:aws:iam::123456789012:role/app",
		},
	}}

	if !predicate.Create(event.CreateEvent{Object: annotated}) {
		t.Fatal("expected create event with managed annotation to be kept")
	}

	if predicate.Delete(event.DeleteEvent{Object: annotated}) {
		t.Fatal("expected delete event with managed annotation to be dropped")
	}

	if !predicate.Generic(event.GenericEvent{Object: annotated}) {
		t.Fatal("expected generic event with managed annotation to be kept")
	}
}

func TestAnnotationRepairPredicateDropsUnmanagedServiceAccounts(t *testing.T) {
	predicate := annotationRepairPredicate()
	unmanaged := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}

	if predicate.Create(event.CreateEvent{Object: unmanaged}) {
		t.Fatal("expected create event without managed annotation to be dropped")
	}

	if predicate.Update(event.UpdateEvent{ObjectOld: unmanaged, ObjectNew: unmanaged.DeepCopy()}) {
		t.Fatal("expected update event without managed annotation to be dropped")
	}
}

func TestAnnotationRepairPredicateKeepsAnnotationRemovalUpdate(t *testing.T) {
	predicate := annotationRepairPredicate()
	oldSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      "app",
		Namespace: "default",
		Annotations: map[string]string{
			selfHostedRoleARNAnnotation: "arn:aws:iam::123456789012:role/app",
		},
	}}
	newSA := oldSA.DeepCopy()
	delete(newSA.Annotations, selfHostedRoleARNAnnotation)

	if !predicate.Update(event.UpdateEvent{ObjectOld: oldSA, ObjectNew: newSA}) {
		t.Fatal("expected annotation removal update to be kept")
	}
}

func TestReconcileRemoteServiceAccountUsesIndexedRoleLookup(t *testing.T) {
	role := roleForServiceAccount("app", "app")
	role.Status.RoleARN = "arn:aws:iam::123456789012:role/app"
	reconciler := &SelfHostedServiceAccountReconciler{LocalClient: testConfigClient(t, role)}
	remoteClient := fakeClient(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}})

	if err := reconciler.reconcileRemoteServiceAccount(context.Background(), logr.Discard(), remoteClient, testInventoryNamespace, types.NamespacedName{Namespace: "default", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	if sa.Annotations[selfHostedRoleARNAnnotation] != role.Status.RoleARN {
		t.Fatalf("expected ServiceAccount role annotation %q, got %#v", role.Status.RoleARN, sa.Annotations)
	}
}

func TestReconcileRemoteServiceAccountSkipsUnmatchedRole(t *testing.T) {
	role := roleForServiceAccount("app", "app")
	role.Status.RoleARN = "arn:aws:iam::123456789012:role/app"
	reconciler := &SelfHostedServiceAccountReconciler{LocalClient: testConfigClient(t, role)}
	remoteClient := fakeClient(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}})

	if err := reconciler.reconcileRemoteServiceAccount(context.Background(), logr.Discard(), remoteClient, testInventoryNamespace, types.NamespacedName{Namespace: "default", Name: "other"}); err != nil {
		t.Fatal(err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	if _, ok := sa.Annotations[selfHostedRoleARNAnnotation]; ok {
		t.Fatalf("expected unmatched ServiceAccount to remain unannotated, got %#v", sa.Annotations)
	}
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()

	for _, add := range []func(*runtime.Scheme) error{
		admissionregistrationv1.AddToScheme,
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
		identityv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		Build()
}
