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
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
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
	}, testRoleARN)
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
				selfHostedRoleARNAnnotation:         testRoleARN,
				selfHostedAudienceAnnotation:        "sts.amazonaws.com",
				selfHostedRegionalSTSAnnotation:     "true",
				selfHostedTokenExpirationAnnotation: "86400",
			},
		},
	})

	op, err := removeRemoteServiceAccountAnnotations(context.Background(), c, identityv1.ServiceAccountSubject{
		Namespace: "default",
		Name:      "app",
	}, testRoleARN)
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
			selfHostedRoleARNAnnotation: testRoleARN,
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
			selfHostedRoleARNAnnotation: testRoleARN,
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
	role.Status.RoleARN = testRoleARN
	recorder := &capturingEventRecorder{}
	reconciler := &SelfHostedServiceAccountReconciler{LocalClient: testConfigClient(t, role), Recorder: recorder}
	remoteClient := fakeClient(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}})
	beforeEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))
	beforeSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))

	if err := reconciler.reconcileRemoteServiceAccount(context.Background(), logr.Discard(), remoteClient, identityv1.DeliveryTypeEKSIRSA, testInventoryNamespace, types.NamespacedName{Namespace: "default", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	if sa.Annotations[selfHostedRoleARNAnnotation] != role.Status.RoleARN {
		t.Fatalf("expected ServiceAccount role annotation %q, got %#v", role.Status.RoleARN, sa.Annotations)
	}

	if len(recorder.events) != 1 {
		t.Fatalf("expected 1 annotation-repair event, got %d: %#v", len(recorder.events), recorder.events)
	}

	got := recorder.events[0]

	gotRole, ok := got.regarding.(*identityv1.AWSServiceAccountRole)
	if !ok {
		t.Fatalf("expected event to regard *AWSServiceAccountRole, got %T", got.regarding)
	}

	if gotRole.Name != role.Name || gotRole.Namespace != role.Namespace {
		t.Fatalf("expected event regarding role %s/%s, got %s/%s", role.Namespace, role.Name, gotRole.Namespace, gotRole.Name)
	}

	if got.eventType != corev1.EventTypeNormal ||
		got.reason != identityv1.ReasonAnnotationRepaired ||
		got.action != eventActionRepairAnnotation ||
		got.note != eventNoteAnnotationRepaired {
		t.Fatalf("unexpected event metadata: %#v", got)
	}

	afterEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))

	afterSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))
	if got := afterEKSIRSA - beforeEKSIRSA; got != 1 {
		t.Fatalf("expected EKSIRSA remote-apply metric delta 1, got %v", got)
	}

	if got := afterSelfHosted - beforeSelfHosted; got != 0 {
		t.Fatalf("expected SelfHostedIRSA remote-apply metric delta 0, got %v", got)
	}
}

func TestReconcileRemoteServiceAccountNoEventWhenAnnotationsAlreadyMatch(t *testing.T) {
	role := roleForServiceAccount("app", "app")
	role.Status.RoleARN = testRoleARN
	recorder := &capturingEventRecorder{}
	reconciler := &SelfHostedServiceAccountReconciler{LocalClient: testConfigClient(t, role), Recorder: recorder}
	remoteClient := fakeClient(t, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "app",
			Namespace:   "default",
			Annotations: renderSelfHostedServiceAccountAnnotations(testRoleARN),
		},
	})

	if err := reconciler.reconcileRemoteServiceAccount(context.Background(), logr.Discard(), remoteClient, identityv1.DeliveryTypeSelfHostedIRSA, testInventoryNamespace, types.NamespacedName{Namespace: "default", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	if len(recorder.events) != 0 {
		t.Fatalf("expected no event when annotations already match; got %#v", recorder.events)
	}
}

func TestReconcileRemoteServiceAccountSkipsMultipleActiveRoles(t *testing.T) {
	roleA := roleForServiceAccount("role-a", "app")
	roleA.Status.RoleARN = "arn:aws:iam::123456789012:role/role-a"
	roleB := roleForServiceAccount("role-b", "app")
	roleB.Status.RoleARN = "arn:aws:iam::123456789012:role/role-b"

	recorder := &capturingEventRecorder{}
	reconciler := &SelfHostedServiceAccountReconciler{LocalClient: testConfigClient(t, roleA, roleB), Recorder: recorder}
	remoteClient := fakeClient(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}})

	entries := []capturedInfoLogEntry{}
	log := logr.New(captureInfoLogSink{entries: &entries})

	if err := reconciler.reconcileRemoteServiceAccount(context.Background(), log, remoteClient, identityv1.DeliveryTypeSelfHostedIRSA, testInventoryNamespace, types.NamespacedName{Namespace: "default", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	if _, ok := sa.Annotations[selfHostedRoleARNAnnotation]; ok {
		t.Fatalf("expected no annotation to be written when multiple active roles bind the SA, got %#v", sa.Annotations)
	}

	// Event.InvolvedObject is a typed reference scoped to a single API server,
	// so the hub recorder must NOT emit an event whose subject is a remote-cluster
	// ServiceAccount. The conflict is surfaced via the local AWSServiceAccountRole
	// ConditionDeliveryReady=False / ReasonInvalidSpec transition instead.
	if len(recorder.events) != 0 {
		t.Fatalf("expected no cross-cluster events recorded on the hub, got %d: %#v", len(recorder.events), recorder.events)
	}

	var skipEntry *capturedInfoLogEntry

	for i := range entries {
		if entries[i].msg == logMsgSkipSARepairMultiRole {
			skipEntry = &entries[i]

			break
		}
	}

	if skipEntry == nil {
		t.Fatalf("expected the multi-role conflict skip to be logged via Info; got entries=%#v", entries)
	}

	// The structured log key must carry both role keys in lexicographic order so
	// the line is stable across reconciles regardless of indexer iteration order.
	wantRoles := []string{testInventoryNamespace + "/role-a", testInventoryNamespace + "/role-b"}

	gotRoles, ok := logValue(skipEntry.values, logKeyConflictingRoles).([]string)
	if !ok {
		t.Fatalf("expected %q log value to be []string, got %#v", logKeyConflictingRoles, logValue(skipEntry.values, logKeyConflictingRoles))
	}

	if len(gotRoles) != len(wantRoles) || gotRoles[0] != wantRoles[0] || gotRoles[1] != wantRoles[1] {
		t.Fatalf("expected lex-sorted role keys %v, got %v", wantRoles, gotRoles)
	}
}

// logValue returns the value associated with key in a logr keysAndValues slice,
// or nil if absent. It mirrors assertLogValue's traversal but supports values
// that are not comparable with ==.
func logValue(values []any, key string) any {
	for i := 0; i+1 < len(values); i += 2 {
		if values[i] == key {
			return values[i+1]
		}
	}

	return nil
}

func TestReconcileRemoteServiceAccountIgnoresDeletingRoleWhenSingleActiveRemains(t *testing.T) {
	now := metav1.Now()
	deletingRole := roleForServiceAccount("role-deleting", "app")
	deletingRole.Status.RoleARN = "arn:aws:iam::123456789012:role/role-deleting"
	// fake client requires a finalizer for DeletionTimestamp to round-trip
	deletingRole.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}
	deletingRole.DeletionTimestamp = &now

	activeRole := roleForServiceAccount("role-active", "app")
	activeRole.Status.RoleARN = testRoleARN

	recorder := &capturingEventRecorder{}
	reconciler := &SelfHostedServiceAccountReconciler{LocalClient: testConfigClient(t, deletingRole, activeRole), Recorder: recorder}
	remoteClient := fakeClient(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}})

	if err := reconciler.reconcileRemoteServiceAccount(context.Background(), logr.Discard(), remoteClient, identityv1.DeliveryTypeSelfHostedIRSA, testInventoryNamespace, types.NamespacedName{Namespace: "default", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	if sa.Annotations[selfHostedRoleARNAnnotation] != activeRole.Status.RoleARN {
		t.Fatalf("expected SA annotation to come from the single active role %q, got %#v", activeRole.Status.RoleARN, sa.Annotations)
	}

	if len(recorder.events) != 1 {
		t.Fatalf("expected 1 annotation-repair event, got %d: %#v", len(recorder.events), recorder.events)
	}

	got := recorder.events[0]
	if got.eventType != corev1.EventTypeNormal || got.reason != identityv1.ReasonAnnotationRepaired {
		t.Fatalf("expected normal annotation-repair event, got %#v", got)
	}

	gotRole, ok := got.regarding.(*identityv1.AWSServiceAccountRole)
	if !ok {
		t.Fatalf("expected event to regard *AWSServiceAccountRole, got %T", got.regarding)
	}

	if gotRole.Name != activeRole.Name {
		t.Fatalf("expected repair event to regard active role %q, got %q", activeRole.Name, gotRole.Name)
	}
}

func TestReconcileRemoteServiceAccountSkipsUnmatchedRole(t *testing.T) {
	role := roleForServiceAccount("app", "app")
	role.Status.RoleARN = testRoleARN
	reconciler := &SelfHostedServiceAccountReconciler{LocalClient: testConfigClient(t, role)}
	remoteClient := fakeClient(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}})

	if err := reconciler.reconcileRemoteServiceAccount(context.Background(), logr.Discard(), remoteClient, identityv1.DeliveryTypeSelfHostedIRSA, testInventoryNamespace, types.NamespacedName{Namespace: "default", Name: "other"}); err != nil {
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

func TestTrimRemoteServiceAccountForCacheDropsUnusedFields(t *testing.T) {
	transform := trimRemoteServiceAccountForCache()
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: "default",
			Annotations: map[string]string{
				selfHostedRoleARNAnnotation: testRoleARN,
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Secrets:          []corev1.ObjectReference{{Name: "token"}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry"}},
	}

	out, err := transform(sa.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}

	trimmed, ok := out.(*corev1.ServiceAccount)
	if !ok {
		t.Fatalf("expected *corev1.ServiceAccount, got %T", out)
	}

	if len(trimmed.Secrets) != 0 || len(trimmed.ImagePullSecrets) != 0 || len(trimmed.ManagedFields) != 0 {
		t.Fatalf("expected unused cache fields to be stripped, got secrets=%#v imagePullSecrets=%#v managedFields=%#v", trimmed.Secrets, trimmed.ImagePullSecrets, trimmed.ManagedFields)
	}

	if trimmed.Annotations[selfHostedRoleARNAnnotation] == "" {
		t.Fatalf("expected annotations to be preserved, got %#v", trimmed.Annotations)
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
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		Build()
}
