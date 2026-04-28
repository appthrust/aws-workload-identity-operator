package controller

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

const (
	testWebhookImageNew     = "example.com/webhook:new"
	testWebhookImageOld     = "example.com/webhook:old"
	testWebhookSidecarImage = "example.com/sidecar:keep"

	testWebhookKeepLabelKey      = "example.com/keep"
	testWebhookKeepLabelMetadata = "metadata"
	testWebhookKeepLabelTemplate = "template"
)

func TestApplyRemoteWebhookRuntimeCreatesMinimalResources(t *testing.T) {
	c := fakeClient(t, availableWebhookDeployment())

	result, err := applyRemoteWebhookRuntime(context.Background(), logr.Discard(), c, c, testWebhookNamespace, testPodIdentityWebhookImage)
	if err != nil {
		t.Fatal(err)
	}

	if !result.Condition.Ready {
		t.Fatalf("expected runtime to become ready, got %s: %s", result.Condition.Reason, result.Condition.Message)
	}

	assertWebhookNamespaceAndCoreResources(t, c)
	assertWebhookTLSSecret(t, c)
	assertWebhookCASecret(t, c)
	assertWebhookDeployment(t, c)
	assertWebhookMutatingWebhookConfiguration(t, c)
	assertWebhookPodTemplateExcludedFromMutation(t, c)
}

// Webhook runtime is shared cluster-wide and must not encode per-Config
// identity into any runtime resource. Repeated applies must produce stable
// labels/annotations regardless of which Config triggered the reconcile.
func TestApplyRemoteWebhookRuntimeLabelsAreStableAcrossApplies(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t, availableWebhookDeployment())

	if _, err := applyRemoteWebhookRuntime(ctx, logr.Discard(), c, c, testWebhookNamespace, testPodIdentityWebhookImage); err != nil {
		t.Fatal(err)
	}

	first := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(first), first); err != nil {
		t.Fatal(err)
	}

	firstLabels := maps.Clone(first.Labels)
	firstPodLabels := maps.Clone(first.Spec.Template.Labels)
	firstPodAnnotations := maps.Clone(first.Spec.Template.Annotations)

	if _, err := applyRemoteWebhookRuntime(ctx, logr.Discard(), c, c, testWebhookNamespace, testPodIdentityWebhookImage); err != nil {
		t.Fatal(err)
	}

	second := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), second); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(firstLabels, second.Labels) {
		t.Fatalf("expected Deployment labels to be stable across applies: first=%#v second=%#v", firstLabels, second.Labels)
	}

	if !reflect.DeepEqual(firstPodLabels, second.Spec.Template.Labels) {
		t.Fatalf("expected PodTemplate labels to be stable across applies: first=%#v second=%#v", firstPodLabels, second.Spec.Template.Labels)
	}

	if !reflect.DeepEqual(firstPodAnnotations, second.Spec.Template.Annotations) {
		t.Fatalf("expected PodTemplate annotations to be stable across applies: first=%#v second=%#v", firstPodAnnotations, second.Spec.Template.Annotations)
	}
}

func TestApplyRemoteWebhookRuntimePreservesExistingResourceLabels(t *testing.T) {
	ctx := context.Background()
	deployment := availableWebhookDeployment()
	// Preserve the managed-by/runtime label set from the fixture (apply-side
	// adoption guard requires it) while overlaying unrelated bookkeeping labels
	// that the reconcile must keep untouched.
	maps.Copy(deployment.Labels, map[string]string{
		identityv1.LabelConfigUID:   "old-config",
		identityv1.LabelInventoryNS: "old-namespace",
		testWebhookKeepLabelKey:     testWebhookKeepLabelMetadata,
	})
	maps.Copy(deployment.Spec.Template.Labels, map[string]string{
		identityv1.LabelConfigUID:   "old-config",
		identityv1.LabelInventoryNS: "old-namespace",
		testWebhookKeepLabelKey:     testWebhookKeepLabelTemplate,
	})
	c := fakeClient(t, deployment)

	if _, err := applyRemoteWebhookRuntime(ctx, logr.Discard(), c, c, testWebhookNamespace, testPodIdentityWebhookImage); err != nil {
		t.Fatal(err)
	}

	stored := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		identityv1.LabelConfigUID:   "old-config",
		identityv1.LabelInventoryNS: "old-namespace",
		testWebhookKeepLabelKey:     testWebhookKeepLabelMetadata,
	} {
		if stored.Labels[key] != want {
			t.Fatalf("expected Deployment label %s=%q to be preserved, got labels %#v", key, want, stored.Labels)
		}
	}

	for key, want := range map[string]string{
		identityv1.LabelConfigUID:   "old-config",
		identityv1.LabelInventoryNS: "old-namespace",
		testWebhookKeepLabelKey:     testWebhookKeepLabelTemplate,
	} {
		if stored.Spec.Template.Labels[key] != want {
			t.Fatalf("expected PodTemplate label %s=%q to be preserved, got labels %#v", key, want, stored.Spec.Template.Labels)
		}
	}
}

func TestApplyRemoteWebhookRuntimeWaitsForDeploymentBeforeCreatingWebhookConfiguration(t *testing.T) {
	c := fakeClient(t)

	result, err := applyRemoteWebhookRuntime(context.Background(), logr.Discard(), c, c, testWebhookNamespace, testPodIdentityWebhookImage)
	if err != nil {
		t.Fatal(err)
	}

	if result.Condition.Ready || result.Condition.Reason != identityv1.ReasonWebhookDeploymentRolloutInProgress {
		t.Fatalf("expected waiting-for-deployment result, got ready=%t reason=%s", result.Condition.Ready, result.Condition.Reason)
	}

	webhook := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(webhook), webhook); err == nil {
		t.Fatal("expected MutatingWebhookConfiguration not to be created before Deployment is Available")
	}
}

// TestApplyRemoteWebhookRuntimeSurfacesUnmanagedMWHCBeforeDeploymentAvailable pins
// the fix for the bug where finishRemoteWebhookAdmission probed the existence of
// the pod-identity-webhook MutatingWebhookConfiguration through the cached
// remoteClient. The remote cache is selector-scoped to the operator's
// managed-by/runtime labels (SelfHostedWebhookRuntimeCacheByObject), so a
// pre-existing unlabeled MWHC named "pod-identity-webhook" (e.g. left behind by
// the upstream amazon-eks-pod-identity-webhook chart) is invisible to the cache.
// Under the previous behavior the existence gate returned (false, nil) and the
// function short-circuited with a "waiting for Deployment" Condition, silently
// masking the unmanaged singleton. The fix routes the existence probe through
// the uncached apiReader, so the gate sees the MWHC, falls through to
// ensureRemoteMutatingWebhookConfiguration, and surfaces errUnmanagedRemoteRuntime
// via createOrUpdateAdoptableWebhookRuntime even while the Deployment is not yet
// Available. This test simulates that exact split by giving applyRemoteWebhookRuntime
// two distinct readers: an apiReader seeded with the unlabeled MWHC and an
// empty remoteClient that mirrors what the selector-scoped cache would expose.
func TestApplyRemoteWebhookRuntimeSurfacesUnmanagedMWHCBeforeDeploymentAvailable(t *testing.T) {
	ctx := context.Background()

	// remoteClient simulates the selector-scoped remote cache: it does NOT
	// contain the pre-existing unlabeled MWHC because the cache selector would
	// filter it out. The Deployment created during apply will live here, but
	// the fake client never transitions it to Available, so the existence gate
	// in finishRemoteWebhookAdmission runs.
	remoteClient := fakeClient(t)

	// apiReader simulates the live API server: it serves the unlabeled MWHC
	// regardless of the operator's selector scope.
	unlabeledMWHC := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName},
	}
	apiReader := fake.NewClientBuilder().
		WithScheme(testWebhookRuntimeScheme(t)).
		WithObjects(unlabeledMWHC).
		Build()

	_, err := applyRemoteWebhookRuntime(ctx, logr.Discard(), apiReader, remoteClient, testWebhookNamespace, testPodIdentityWebhookImage)
	if err == nil {
		t.Fatal("expected applyRemoteWebhookRuntime to surface errUnmanagedRemoteRuntime when a pre-existing unlabeled MWHC is visible only via apiReader, got nil error")
	}

	if !errors.Is(err, errUnmanagedRemoteRuntime) {
		t.Fatalf("expected error to satisfy errors.Is(err, errUnmanagedRemoteRuntime); the existence gate must probe via apiReader so a pre-existing unlabeled MWHC is not silently masked while the Deployment is not yet Available, got %q", err.Error())
	}

	// The unlabeled MWHC must remain untouched on the live API server — the
	// adoption guard's whole job is to refuse to mutate it.
	gotMWHC := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if getErr := apiReader.Get(ctx, client.ObjectKeyFromObject(unlabeledMWHC), gotMWHC); getErr != nil {
		t.Fatalf("expected unlabeled MWHC to be preserved on apiReader after refused apply, got %v", getErr)
	}

	if _, hasManagedBy := gotMWHC.Labels[identityv1.LabelManagedBy]; hasManagedBy {
		t.Fatalf("expected pre-existing MWHC to remain unlabeled after refused apply, got labels %#v", gotMWHC.Labels)
	}

	if _, hasRuntime := gotMWHC.Labels[identityv1.LabelRuntime]; hasRuntime {
		t.Fatalf("expected pre-existing MWHC to remain unlabeled after refused apply, got labels %#v", gotMWHC.Labels)
	}
}

// In-namespace Role/RoleBinding are no longer applied; only ClusterRole/ClusterRoleBinding remain.
func TestApplyRemoteSupportingResourcesDoesNotCreateNamespacedRBAC(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t)

	if err := applyRemoteSupportingResources(ctx, logr.Discard(), c, c, testWebhookNamespace); err != nil {
		t.Fatal(err)
	}

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(role), role); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no Role to be created in remote namespace, got err=%v role=%#v", err, role)
	}

	roleList := &rbacv1.RoleList{}
	if err := c.List(ctx, roleList, client.InNamespace(testWebhookNamespace)); err != nil {
		t.Fatal(err)
	}

	if len(roleList.Items) != 0 {
		t.Fatalf("expected no Roles in remote namespace %q, got %d: %#v", testWebhookNamespace, len(roleList.Items), roleList.Items)
	}

	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(roleBinding), roleBinding); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no RoleBinding to be created in remote namespace, got err=%v roleBinding=%#v", err, roleBinding)
	}

	roleBindingList := &rbacv1.RoleBindingList{}
	if err := c.List(ctx, roleBindingList, client.InNamespace(testWebhookNamespace)); err != nil {
		t.Fatal(err)
	}

	if len(roleBindingList.Items) != 0 {
		t.Fatalf("expected no RoleBindings in remote namespace %q, got %d: %#v", testWebhookNamespace, len(roleBindingList.Items), roleBindingList.Items)
	}
}

func TestEnsureRemoteNamespaceDoesNotAdoptExistingNamespace(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   testWebhookNamespace,
		Labels: map[string]string{"example.com/owner": "user"},
	}})

	op, err := ensureRemoteNamespace(ctx, c, c, testWebhookNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultNone {
		t.Fatalf("expected existing Namespace to be left unchanged, got operation %s", op)
	}

	stored := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	if stored.Labels["example.com/owner"] != "user" {
		t.Fatalf("expected user Namespace labels to be preserved, got %#v", stored.Labels)
	}

	if _, ok := stored.Labels[identityv1.LabelManagedBy]; ok {
		t.Fatalf("expected existing Namespace not to be adopted, got labels %#v", stored.Labels)
	}
}

func TestEnsureRemoteNamespacePreservesExistingResourceLabels(t *testing.T) {
	ctx := context.Background()
	existingLabels := webhookRuntimeLabels()
	existingLabels[identityv1.LabelConfigUID] = "old-config"
	existingLabels[identityv1.LabelInventoryNS] = "old-namespace"
	existingLabels[testWebhookKeepLabelKey] = testWebhookKeepLabelMetadata
	c := fakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   testWebhookNamespace,
		Labels: existingLabels,
	}})

	op, err := ensureRemoteNamespace(ctx, c, c, testWebhookNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultNone {
		t.Fatalf("expected managed Namespace labels to be unchanged, got operation %s", op)
	}

	stored := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		identityv1.LabelConfigUID:   "old-config",
		identityv1.LabelInventoryNS: "old-namespace",
		testWebhookKeepLabelKey:     testWebhookKeepLabelMetadata,
	} {
		if stored.Labels[key] != want {
			t.Fatalf("expected Namespace label %s=%q to be preserved, got labels %#v", key, want, stored.Labels)
		}
	}

	if stored.Labels[testWebhookKeepLabelKey] != testWebhookKeepLabelMetadata {
		t.Fatalf("expected unrelated Namespace label to be preserved, got %#v", stored.Labels)
	}
}

// TestEnsureRemoteNamespaceRetriesOnConflict pins that ensureRemoteNamespace
// goes through the createOrUpdate helper's retry.RetryOnConflict loop: when
// the first Update returns Conflict, the helper must Get-and-Update again
// rather than surfacing the error. The previous body called c.Update directly
// with no retry; this regression test would fail under that implementation.
func TestEnsureRemoteNamespaceRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	existingLabels := map[string]string{
		identityv1.LabelManagedBy: identityv1.ManagedByValue,
		identityv1.LabelRuntime:   identityv1.RuntimeWebhook,
		identityv1.LabelConfigUID: "old-config",
		testWebhookKeepLabelKey:   testWebhookKeepLabelMetadata,
	}
	seed := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   testWebhookNamespace,
		Labels: existingLabels,
	}}

	scheme := testWebhookRuntimeScheme(t)

	var updateCalls int

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(seed).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updateCalls++
				if updateCalls == 1 {
					return apierrors.NewConflict(corev1.Resource("namespaces"), obj.GetName(), nil)
				}

				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	op, err := ensureRemoteNamespace(ctx, c, c, testWebhookNamespace)
	if err != nil {
		t.Fatalf("expected ensureRemoteNamespace to retry past Conflict, got %v", err)
	}

	if op != controllerutil.OperationResultUpdated {
		t.Fatalf("expected managed Namespace to be updated after retry, got operation %s", op)
	}

	if updateCalls < 2 {
		t.Fatalf("expected at least 2 Update calls (one Conflict + one success), got %d", updateCalls)
	}

	stored := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	if stored.Labels[identityv1.LabelConfigUID] != "old-config" {
		t.Fatalf("expected existing Config UID label to be preserved through retry loop, got %#v", stored.Labels)
	}

	assertWebhookRuntimeLabels(t, stored)

	if stored.Labels[testWebhookKeepLabelKey] != testWebhookKeepLabelMetadata {
		t.Fatalf("expected unrelated Namespace label to be preserved, got %#v", stored.Labels)
	}
}

func TestDeleteRemoteWebhookRuntimePreservesExistingNamespace(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   testWebhookNamespace,
			Labels: map[string]string{"example.com/owner": "user"},
		}},
		&appsv1.Deployment{ObjectMeta: managedWebhookRuntimeObjectMeta(testWebhookNamespace)},
		&corev1.Service{ObjectMeta: managedWebhookRuntimeObjectMeta(testWebhookNamespace)},
		&corev1.Secret{ObjectMeta: managedWebhookRuntimeObjectMeta(testWebhookNamespace)},
		&corev1.ServiceAccount{ObjectMeta: managedWebhookRuntimeObjectMeta(testWebhookNamespace)},
		&rbacv1.Role{ObjectMeta: managedWebhookRuntimeObjectMeta(testWebhookNamespace)},
		&rbacv1.RoleBinding{ObjectMeta: managedWebhookRuntimeObjectMeta(testWebhookNamespace)},
		&rbacv1.ClusterRole{ObjectMeta: managedWebhookRuntimeObjectMeta("")},
	)

	if err := deleteRemoteWebhookRuntime(ctx, c, c, testWebhookNamespace); err != nil {
		t.Fatal(err)
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatalf("expected existing Namespace to be preserved: %v", err)
	}

	if ns.Labels["example.com/owner"] != "user" {
		t.Fatalf("expected preserved Namespace labels, got %#v", ns.Labels)
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); !apierrors.IsNotFound(err) {
		t.Fatalf("expected managed Deployment to be deleted, got %v", err)
	}

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(role), role); err != nil {
		t.Fatalf("expected managed Role to be left alone: %v", err)
	}

	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(roleBinding), roleBinding); err != nil {
		t.Fatalf("expected managed RoleBinding to be left alone: %v", err)
	}

	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(clusterRole), clusterRole); !apierrors.IsNotFound(err) {
		t.Fatalf("expected managed ClusterRole to be deleted, got %v", err)
	}
}

func TestDeleteRemoteWebhookRuntimePreservesUnmanagedNamespacedObjects(t *testing.T) {
	ctx := context.Background()
	unmanagedLabels := map[string]string{"example.com/owner": "user"}
	objects := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
	}
	c := fakeClient(t, objects...)

	if err := deleteRemoteWebhookRuntime(ctx, c, c, testWebhookNamespace); err != nil {
		t.Fatal(err)
	}

	for _, obj := range objects {
		key := client.ObjectKeyFromObject(obj)
		if err := c.Get(ctx, key, obj); err != nil {
			t.Fatalf("expected unmanaged %T %s to be preserved: %v", obj, key, err)
		}
	}
}

func TestDeleteRemoteWebhookRuntimeBlocksOnUnmanagedClusterScopedSingleton(t *testing.T) {
	unmanagedLabels := map[string]string{"example.com/owner": "user"}

	cases := []struct {
		name string
		obj  client.Object
	}{
		{
			name: "MutatingWebhookConfiguration",
			obj:  &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
		},
		{
			name: "ClusterRoleBinding",
			obj:  &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
		},
		{
			name: "ClusterRole",
			obj:  &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := fakeClient(t, tc.obj)

			err := deleteRemoteWebhookRuntime(ctx, c, c, testWebhookNamespace)
			if err == nil {
				t.Fatalf("expected deleteRemoteWebhookRuntime to return an error for unmanaged cluster-scoped %T, got nil", tc.obj)
			}

			if !errors.Is(err, errUnmanagedRemoteRuntime) {
				t.Fatalf("expected error to satisfy errors.Is(err, errUnmanagedRemoteRuntime), got %q", err.Error())
			}

			if !strings.Contains(err.Error(), "force-delete") {
				t.Fatalf("expected error to mention force-delete bypass, got %q", err.Error())
			}

			key := client.ObjectKeyFromObject(tc.obj)
			if getErr := c.Get(ctx, key, tc.obj); getErr != nil {
				t.Fatalf("expected unmanaged cluster-scoped %T %s to be preserved after blocked delete, got %v", tc.obj, key, getErr)
			}
		})
	}
}

// TestDeleteRemoteWebhookRuntimeUsesUncachedReaderForOwnershipCheck pins the
// fix for the bug where deleteManagedRemoteWebhookRuntimeObject used to query
// the selector-scoped remote cache for the ownership check. A foreign or
// label-drifted cluster-scoped MutatingWebhookConfiguration named
// "pod-identity-webhook" without our managed-by/runtime labels is filtered out
// by the cache (returning NotFound), so the function would silently return nil
// and bypass the errUnmanagedRemoteRuntime guardrail. The fix routes the
// ownership Get through an uncached apiReader. This test simulates that exact
// split by handing the function an EMPTY cacheView and an apiReader seeded
// with the unmanaged singleton.
func TestDeleteRemoteWebhookRuntimeUsesUncachedReaderForOwnershipCheck(t *testing.T) {
	ctx := context.Background()
	unmanagedLabels := map[string]string{"example.com/owner": "user"}
	unmanagedMWHC := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)},
	}

	// cacheView simulates the selector-scoped remote cache that filters out
	// objects lacking the managed-by/runtime labels. It is intentionally EMPTY
	// — the unmanaged singleton would not satisfy the cache selector.
	cacheView := fakeClient(t)
	// apiReader simulates the live API server, which still serves the
	// unmanaged singleton regardless of label selectors.
	apiReader := fakeClient(t, unmanagedMWHC)

	err := deleteRemoteWebhookRuntime(ctx, apiReader, cacheView, testWebhookNamespace)
	if err == nil {
		t.Fatal("expected deleteRemoteWebhookRuntime to return an error for unmanaged cluster-scoped MWHC visible only via apiReader, got nil")
	}

	if !errors.Is(err, errUnmanagedRemoteRuntime) {
		t.Fatalf("expected error to satisfy errors.Is(err, errUnmanagedRemoteRuntime), got %q", err.Error())
	}

	if !strings.Contains(err.Error(), "force-delete") {
		t.Fatalf("expected error to mention force-delete bypass, got %q", err.Error())
	}

	// The unmanaged MWHC must still be present in the live API after the
	// blocked delete — the guardrail's whole job is to leave it alone.
	gotMWHC := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if getErr := apiReader.Get(ctx, client.ObjectKeyFromObject(unmanagedMWHC), gotMWHC); getErr != nil {
		t.Fatalf("expected unmanaged cluster-scoped MWHC to be preserved in apiReader after blocked delete, got %v", getErr)
	}

	// Negative control: had the old behavior been used (Get against the
	// selector-scoped cache instead of apiReader), the cache would have
	// returned NotFound and the function would have silently returned nil.
	// Pinning this here makes the regression's mechanism explicit for future
	// readers.
	t.Run("CacheViewReturnsNotFoundForUnmanagedSingleton", func(t *testing.T) {
		viaCache := &admissionregistrationv1.MutatingWebhookConfiguration{}
		if cacheErr := cacheView.Get(ctx, client.ObjectKeyFromObject(unmanagedMWHC), viaCache); !apierrors.IsNotFound(cacheErr) {
			t.Fatalf("expected selector-scoped cacheView to return NotFound for unmanaged MWHC (the precondition for the regression), got %v", cacheErr)
		}
	})
}

// applyAdoptionGuardCase shares structure between the namespaced and
// cluster-scoped apply-side adoption guard tests below. seed is the unmanaged
// pre-existing object placed in the fake client; invoke calls the ensure*
// helper under test; fetch returns an empty pointer of the same concrete type
// for re-Get after the refused apply; verify runs kind-specific assertions on
// the post-apply object snapshot (e.g. that the MWHC Webhooks slice is
// unchanged).
type applyAdoptionGuardCase struct {
	name   string
	seed   client.Object
	invoke func(context.Context, client.Client) error
	fetch  func() client.Object
	verify func(t *testing.T, stored client.Object)
}

// testWebhookForeignOwnerLabel / testWebhookForeignOwnerValue stand in for the
// "user installed by an upstream chart" label set that the apply-side
// adoption guard must preserve verbatim.
const (
	testWebhookForeignOwnerLabel = "example.com/owner"
	testWebhookForeignOwnerValue = "user"
)

func runApplyAdoptionGuardCase(t *testing.T, tc applyAdoptionGuardCase) {
	t.Helper()

	ctx := context.Background()
	c := fakeClient(t, tc.seed)

	err := tc.invoke(ctx, c)
	if err == nil {
		t.Fatalf("expected apply helper to refuse overwriting unmanaged %T, got nil error", tc.seed)
	}

	if !errors.Is(err, errUnmanagedRemoteRuntime) {
		t.Fatalf("expected error to satisfy errors.Is(err, errUnmanagedRemoteRuntime), got %q", err.Error())
	}

	if !strings.Contains(err.Error(), "force-delete") {
		t.Fatalf("expected error to mention force-delete bypass, got %q", err.Error())
	}

	stored := tc.fetch()
	if getErr := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); getErr != nil {
		t.Fatalf("expected unmanaged %T to remain present after refused apply, got %v", stored, getErr)
	}

	gotLabels := stored.GetLabels()
	if gotLabels[testWebhookForeignOwnerLabel] != testWebhookForeignOwnerValue {
		t.Fatalf("expected seeded foreign label %s=%s to be preserved, got labels %#v", testWebhookForeignOwnerLabel, testWebhookForeignOwnerValue, gotLabels)
	}

	if _, ok := gotLabels[identityv1.LabelManagedBy]; ok {
		t.Fatalf("expected refused apply not to stamp managed-by label, got labels %#v", gotLabels)
	}

	if _, ok := gotLabels[identityv1.LabelRuntime]; ok {
		t.Fatalf("expected refused apply not to stamp runtime label, got labels %#v", gotLabels)
	}

	if tc.verify != nil {
		tc.verify(t, stored)
	}
}

// TestApplyRemoteWebhookRuntimeRefusesToOverwriteUnmanagedNamespacedObjects
// pins the apply-side adoption guard for namespaced ensure* helpers: when a
// pre-existing object named "pod-identity-webhook" (or the CA Secret) lives in
// the target namespace WITHOUT the operator's managed-by label (e.g. installed
// by the upstream amazon-eks-pod-identity-webhook Helm chart), each helper
// must refuse to mutate it and surface errUnmanagedRemoteRuntime. The seeded
// foreign label set must remain intact and no managed-by/runtime labels may
// be stamped onto the object.
//
//nolint:funlen // table-driven per-helper cases kept inline; extracting them would obscure the helper / seed / fetch pairing.
func TestApplyRemoteWebhookRuntimeRefusesToOverwriteUnmanagedNamespacedObjects(t *testing.T) {
	unmanagedLabels := map[string]string{testWebhookForeignOwnerLabel: testWebhookForeignOwnerValue}

	caCertPEM, caKeyPEM, err := generateWebhookCACertificate()
	if err != nil {
		t.Fatal(err)
	}

	caCert := mustParseSingleCertificatePEM(t, caCertPEM)
	caKey := mustParseRSAPrivateKeyPEM(t, caKeyPEM)

	cases := []applyAdoptionGuardCase{
		{
			name: "CASecret",
			seed: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, c client.Client) error {
				_, _, _, _, err := ensureRemoteCASecret(ctx, c, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "ServingTLSSecret",
			seed: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, c client.Client) error {
				_, _, _, err := ensureRemoteServingTLSSecret(ctx, c, c, testWebhookNamespace, caCert, caKey)

				return err
			},
			fetch: func() client.Object {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "ServiceAccount",
			seed: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, c client.Client) error {
				_, err := ensureRemoteServiceAccount(ctx, c, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "Service",
			seed: &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, c client.Client) error {
				_, err := ensureRemoteService(ctx, c, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "Deployment",
			seed: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, c client.Client) error {
				_, _, err := ensureRemoteDeployment(ctx, c, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a")

				return err
			},
			fetch: func() client.Object {
				return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runApplyAdoptionGuardCase(t, tc)
		})
	}
}

// TestApplyRemoteWebhookRuntimeRefusesToOverwriteUnmanagedClusterScopedSingletons
// pins the apply-side adoption guard for cluster-scoped ensure* helpers. The
// most consequential collision is an upstream-installed
// MutatingWebhookConfiguration named "pod-identity-webhook": overwriting its
// Webhooks slice would silently re-point cluster-wide Pod admission at this
// operator's Service and CA bundle. The guard must refuse to mutate the
// singleton, return errUnmanagedRemoteRuntime, and leave the seeded Webhooks
// slice intact for the MWHC case.
//
//nolint:funlen // table-driven per-singleton cases kept inline; the MWHC entry carries a kind-specific Webhooks-preservation verify closure that does not factor out cleanly.
func TestApplyRemoteWebhookRuntimeRefusesToOverwriteUnmanagedClusterScopedSingletons(t *testing.T) {
	unmanagedLabels := map[string]string{testWebhookForeignOwnerLabel: testWebhookForeignOwnerValue}

	upstreamWebhooks := []admissionregistrationv1.MutatingWebhook{{
		Name:                    "upstream." + webhookComponentName,
		AdmissionReviewVersions: []string{"v1"},
		SideEffects:             ptr.To(admissionregistrationv1.SideEffectClassNone),
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: []byte("upstream-ca-bundle"),
			Service: &admissionregistrationv1.ServiceReference{
				Name:      "upstream-webhook",
				Namespace: "upstream-namespace",
			},
		},
	}}

	cases := []applyAdoptionGuardCase{
		{
			name: "ClusterRole",
			seed: &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, c client.Client) error {
				_, err := ensureRemoteClusterRole(ctx, c, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
			},
		},
		{
			name: "ClusterRoleBinding",
			seed: &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, c client.Client) error {
				_, err := ensureRemoteClusterRoleBinding(ctx, c, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
			},
		},
		{
			name: "MutatingWebhookConfiguration",
			seed: &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)},
				Webhooks:   upstreamWebhooks,
			},
			invoke: func(ctx context.Context, c client.Client) error {
				_, _, err := ensureRemoteMutatingWebhookConfiguration(ctx, c, c, testWebhookNamespace, []byte("operator-ca-bundle"))

				return err
			},
			fetch: func() client.Object {
				return &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
			},
			verify: func(t *testing.T, stored client.Object) {
				t.Helper()

				mwhc, ok := stored.(*admissionregistrationv1.MutatingWebhookConfiguration)
				if !ok {
					t.Fatalf("expected stored object to be *MutatingWebhookConfiguration, got %T", stored)
				}

				if !reflect.DeepEqual(mwhc.Webhooks, upstreamWebhooks) {
					t.Fatalf("expected upstream Webhooks slice to remain unchanged after refused apply, got %#v want %#v", mwhc.Webhooks, upstreamWebhooks)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runApplyAdoptionGuardCase(t, tc)
		})
	}
}

// applyAdoptionGuardUncachedCase models the per-helper apply-side adoption
// case where the selector-scoped remote cache (cacheView) hides the
// pre-existing unmanaged object and only the uncached apiReader sees it.
// invoke receives the two split clients so each case can call its ensure*
// helper with the exact (apiReader, c) wiring used in production.
type applyAdoptionGuardUncachedCase struct {
	name   string
	seed   client.Object
	invoke func(ctx context.Context, apiReader client.Reader, c client.Client) error
	fetch  func() client.Object
	verify func(t *testing.T, stored client.Object)
}

func runApplyAdoptionGuardUncachedCase(t *testing.T, tc applyAdoptionGuardUncachedCase) {
	t.Helper()

	ctx := context.Background()
	// cacheView simulates the selector-scoped remote cache that filters out
	// objects lacking the managed-by/runtime labels. It is intentionally EMPTY
	// — the unmanaged seed would not satisfy the cache selector.
	cacheView := fakeClient(t)
	// apiReader simulates the live API server, which still serves the
	// unmanaged object regardless of label selectors.
	apiReader := fakeClient(t, tc.seed)

	err := tc.invoke(ctx, apiReader, cacheView)
	if err == nil {
		t.Fatalf("expected apply helper to refuse overwriting unmanaged %T visible only via apiReader, got nil error", tc.seed)
	}

	if !errors.Is(err, errUnmanagedRemoteRuntime) {
		t.Fatalf("expected error to satisfy errors.Is(err, errUnmanagedRemoteRuntime), got %q", err.Error())
	}

	if !strings.Contains(err.Error(), "force-delete") {
		t.Fatalf("expected error to mention force-delete bypass, got %q", err.Error())
	}

	stored := tc.fetch()
	if getErr := apiReader.Get(ctx, client.ObjectKeyFromObject(stored), stored); getErr != nil {
		t.Fatalf("expected unmanaged %T to remain present on apiReader after refused apply, got %v", stored, getErr)
	}

	gotLabels := stored.GetLabels()
	if gotLabels[testWebhookForeignOwnerLabel] != testWebhookForeignOwnerValue {
		t.Fatalf("expected seeded foreign label %s=%s to be preserved on apiReader, got labels %#v", testWebhookForeignOwnerLabel, testWebhookForeignOwnerValue, gotLabels)
	}

	if _, ok := gotLabels[identityv1.LabelManagedBy]; ok {
		t.Fatalf("expected refused apply not to stamp managed-by label on apiReader-side object, got labels %#v", gotLabels)
	}

	if _, ok := gotLabels[identityv1.LabelRuntime]; ok {
		t.Fatalf("expected refused apply not to stamp runtime label on apiReader-side object, got labels %#v", gotLabels)
	}

	if tc.verify != nil {
		tc.verify(t, stored)
	}
}

// TestApplyRemoteWebhookRuntimeUsesUncachedReaderForAdoption pins the
// apply-side equivalent of the deletion-path uncached-reader fix. Every
// webhook-runtime ensure* helper that funnels through
// createOrUpdateAdoptableWebhookRuntime must probe the live API server via the
// uncached apiReader for the ownership decision, not the selector-scoped
// remote cache. Were the probe routed through the cache, a pre-existing
// unlabeled object (e.g. installed by the upstream
// amazon-eks-pod-identity-webhook chart) would be filtered out, the in-mutate
// adoptableForApply check would see a zero-ResourceVersion object on the
// cached Create-or-Update path and pass it as a false-positive, and the
// follow-on Create would collide with the real object on the API server and
// surface as a generic conflict rather than the intended
// errUnmanagedRemoteRuntime sentinel. Each case wires the helper with a split
// (cacheView, apiReader) pair to reproduce that exact failure mode.
//
//nolint:funlen // table-driven per-helper cases kept inline; extracting them would obscure the helper / seed / fetch pairing.
func TestApplyRemoteWebhookRuntimeUsesUncachedReaderForAdoption(t *testing.T) {
	unmanagedLabels := map[string]string{testWebhookForeignOwnerLabel: testWebhookForeignOwnerValue}

	caCertPEM, caKeyPEM, err := generateWebhookCACertificate()
	if err != nil {
		t.Fatal(err)
	}

	caCert := mustParseSingleCertificatePEM(t, caCertPEM)
	caKey := mustParseRSAPrivateKeyPEM(t, caKeyPEM)

	upstreamWebhooks := []admissionregistrationv1.MutatingWebhook{{
		Name:                    "upstream." + webhookComponentName,
		AdmissionReviewVersions: []string{"v1"},
		SideEffects:             ptr.To(admissionregistrationv1.SideEffectClassNone),
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			CABundle: []byte("upstream-ca-bundle"),
			Service: &admissionregistrationv1.ServiceReference{
				Name:      "upstream-webhook",
				Namespace: "upstream-namespace",
			},
		},
	}}

	cases := []applyAdoptionGuardUncachedCase{
		{
			name: "CASecret",
			seed: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, _, _, _, err := ensureRemoteCASecret(ctx, apiReader, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "ServingTLSSecret",
			seed: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, _, _, err := ensureRemoteServingTLSSecret(ctx, apiReader, c, testWebhookNamespace, caCert, caKey)

				return err
			},
			fetch: func() client.Object {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "ServiceAccount",
			seed: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, err := ensureRemoteServiceAccount(ctx, apiReader, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "Service",
			seed: &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, err := ensureRemoteService(ctx, apiReader, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "ClusterRole",
			seed: &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, err := ensureRemoteClusterRole(ctx, apiReader, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
			},
		},
		{
			name: "ClusterRoleBinding",
			seed: &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, err := ensureRemoteClusterRoleBinding(ctx, apiReader, c, testWebhookNamespace)

				return err
			},
			fetch: func() client.Object {
				return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
			},
		},
		{
			name: "Deployment",
			seed: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, _, err := ensureRemoteDeployment(ctx, apiReader, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a")

				return err
			},
			fetch: func() client.Object {
				return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
			},
		},
		{
			name: "MutatingWebhookConfiguration",
			seed: &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)},
				Webhooks:   upstreamWebhooks,
			},
			invoke: func(ctx context.Context, apiReader client.Reader, c client.Client) error {
				_, _, err := ensureRemoteMutatingWebhookConfiguration(ctx, apiReader, c, testWebhookNamespace, []byte("operator-ca-bundle"))

				return err
			},
			fetch: func() client.Object {
				return &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
			},
			verify: func(t *testing.T, stored client.Object) {
				t.Helper()

				mwhc, ok := stored.(*admissionregistrationv1.MutatingWebhookConfiguration)
				if !ok {
					t.Fatalf("expected stored object to be *MutatingWebhookConfiguration, got %T", stored)
				}

				if !reflect.DeepEqual(mwhc.Webhooks, upstreamWebhooks) {
					t.Fatalf("expected upstream Webhooks slice to remain unchanged after refused apply, got %#v want %#v", mwhc.Webhooks, upstreamWebhooks)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runApplyAdoptionGuardUncachedCase(t, tc)
		})
	}
}

// TestEnsureRemoteNamespaceLeavesUnmanagedNamespaceUntouched pins the
// uncached-reader probe baked into ensureRemoteNamespace. The selector-scoped
// remote cache filters out a pre-existing namespace lacking the operator's
// managed-by/runtime labels, so a cached Get would return NotFound and the
// follow-on Create would collide with AlreadyExists on the live API server.
// The fix routes the existence probe through the uncached apiReader, and
// when the namespace exists but is unmanaged the helper must silently skip
// (OperationResultNone, nil) without stamping the operator's labels or
// removing the seeded foreign label.
func TestEnsureRemoteNamespaceLeavesUnmanagedNamespaceUntouched(t *testing.T) {
	ctx := context.Background()

	seededNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   testWebhookNamespace,
			Labels: map[string]string{testWebhookForeignOwnerLabel: testWebhookForeignOwnerValue},
		},
	}

	// cacheView simulates the selector-scoped remote cache: it filters out the
	// unlabeled namespace and so is empty.
	cacheView := fakeClient(t)
	// apiReader simulates the live API server: the namespace exists, unlabeled.
	apiReader := fakeClient(t, seededNS)

	op, err := ensureRemoteNamespace(ctx, apiReader, cacheView, testWebhookNamespace)
	if err != nil {
		t.Fatalf("expected ensureRemoteNamespace to silently skip an unmanaged namespace, got error %v", err)
	}

	if op != controllerutil.OperationResultNone {
		t.Fatalf("expected OperationResultNone when leaving unmanaged namespace untouched, got %s", op)
	}

	stored := &corev1.Namespace{}
	if getErr := apiReader.Get(ctx, client.ObjectKey{Name: testWebhookNamespace}, stored); getErr != nil {
		t.Fatalf("expected seeded unmanaged namespace to remain present on apiReader, got %v", getErr)
	}

	gotLabels := stored.GetLabels()
	if gotLabels[testWebhookForeignOwnerLabel] != testWebhookForeignOwnerValue {
		t.Fatalf("expected seeded foreign label %s=%s to be preserved, got labels %#v", testWebhookForeignOwnerLabel, testWebhookForeignOwnerValue, gotLabels)
	}

	if _, ok := gotLabels[identityv1.LabelManagedBy]; ok {
		t.Fatalf("expected ensureRemoteNamespace not to stamp managed-by label on unmanaged namespace, got labels %#v", gotLabels)
	}

	if _, ok := gotLabels[identityv1.LabelRuntime]; ok {
		t.Fatalf("expected ensureRemoteNamespace not to stamp runtime label on unmanaged namespace, got labels %#v", gotLabels)
	}
}

func TestEnsureRemoteDeploymentPreservesUnmanagedTemplateFields(t *testing.T) {
	ctx := context.Background()

	c := fakeClient(t, webhookDeploymentWithUnmanagedFields())

	_, op, err := ensureRemoteDeployment(ctx, c, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a")
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultUpdated {
		t.Fatalf("expected Deployment update, got %s", op)
	}

	stored := getWebhookDeployment(ctx, t, c)
	assertUnmanagedDeploymentFieldsPreserved(t, stored)
	assertManagedDeploymentFieldsReconciled(t, stored)
}

func webhookDeploymentWithUnmanagedFields() *appsv1.Deployment {
	// The Deployment is operator-owned (managed-by/runtime labels stamped) but
	// also carries unrelated metadata labels and pod-template fields the
	// operator does not own. The apply path must keep both and only reconcile
	// the operator's contract (selector / container / cert volume). The
	// managed-by label is required for adoptableForApply to permit the update;
	// the example.com/deployment label exercises preservation of unrelated keys.
	labels := webhookRuntimeLabels()
	labels["example.com/deployment"] = testPreservedValue

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookComponentName,
			Namespace: testWebhookNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: webhookDeploymentSelector(),
			Template: webhookPodTemplateWithUnmanagedFields(),
		},
	}
}

func webhookPodTemplateWithUnmanagedFields() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"example.com/pod": testPreservedValue,
			},
			Annotations: map[string]string{
				"example.com/template": testPreservedValue,
			},
		},
		Spec: webhookPodSpecWithUnmanagedFields(),
	}
}

func webhookPodSpecWithUnmanagedFields() corev1.PodSpec {
	return corev1.PodSpec{
		NodeSelector: map[string]string{"disk": "ssd"},
		ImagePullSecrets: []corev1.LocalObjectReference{{
			Name: "pull-secret",
		}},
		Tolerations: []corev1.Toleration{{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    "webhook",
		}},
		InitContainers: []corev1.Container{{
			Name:  "init",
			Image: "example.com/init:keep",
		}},
		Containers: []corev1.Container{
			{
				Name:  "sidecar",
				Image: testWebhookSidecarImage,
			},
			{
				Name:  "webhook",
				Image: testWebhookImageOld,
				Env: []corev1.EnvVar{{
					Name:  "USER_EDIT",
					Value: "not-preserved-inside-managed-container",
				}},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "scratch",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
			{
				Name: "cert",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{},
				},
			},
		},
	}
}

func assertUnmanagedDeploymentFieldsPreserved(t *testing.T, stored *appsv1.Deployment) {
	t.Helper()

	if stored.Labels["example.com/deployment"] != testPreservedValue {
		t.Fatalf("expected unmanaged Deployment label to be preserved, got %#v", stored.Labels)
	}

	if stored.Spec.Template.Labels["example.com/pod"] != testPreservedValue {
		t.Fatalf("expected unmanaged PodTemplate label to be preserved, got %#v", stored.Spec.Template.Labels)
	}

	if stored.Spec.Template.Annotations["example.com/template"] != testPreservedValue {
		t.Fatalf("expected unmanaged PodTemplate annotation to be preserved, got %#v", stored.Spec.Template.Annotations)
	}

	if stored.Spec.Template.Annotations[webhookServingCertFingerprintKey] != "fingerprint-a" {
		t.Fatalf("unexpected PodTemplate annotations: %#v", stored.Spec.Template.Annotations)
	}

	podSpec := stored.Spec.Template.Spec
	if podSpec.NodeSelector["disk"] != "ssd" ||
		len(podSpec.ImagePullSecrets) != 1 ||
		len(podSpec.Tolerations) != 1 ||
		len(podSpec.InitContainers) != 1 {
		t.Fatalf("expected pod tuning fields to be preserved, got %#v", stored.Spec.Template.Spec)
	}
}

func assertManagedDeploymentFieldsReconciled(t *testing.T, stored *appsv1.Deployment) {
	t.Helper()

	webhook, ok := containerByName(stored.Spec.Template.Spec.Containers, "webhook")
	if !ok {
		t.Fatalf("expected managed webhook container, got %#v", stored.Spec.Template.Spec.Containers)
	}

	if webhook.Image != testWebhookImageNew || len(webhook.Env) != 0 {
		t.Fatalf("expected webhook container to be replaced by managed spec, got %#v", webhook)
	}

	assertWebhookContainerSecurityContext(t, webhook)

	sidecar, ok := containerByName(stored.Spec.Template.Spec.Containers, "sidecar")
	if !ok || sidecar.Image != testWebhookSidecarImage {
		t.Fatalf("expected sidecar to be preserved, got %#v", stored.Spec.Template.Spec.Containers)
	}

	cert, ok := volumeByName(stored.Spec.Template.Spec.Volumes, "cert")
	if !ok || cert.Secret == nil || cert.Secret.SecretName != webhookComponentName {
		t.Fatalf("expected cert volume to be replaced by managed Secret volume, got %#v", cert)
	}

	if _, ok := volumeByName(stored.Spec.Template.Spec.Volumes, "scratch"); !ok {
		t.Fatalf("expected extra volume to be preserved, got %#v", stored.Spec.Template.Spec.Volumes)
	}
}

func getWebhookDeployment(ctx context.Context, t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()

	stored := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	return stored
}

func TestEnsureRemoteDeploymentNoopsAfterManagedFieldsConverge(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t)

	if _, _, err := ensureRemoteDeployment(ctx, c, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a"); err != nil {
		t.Fatal(err)
	}

	_, op, err := ensureRemoteDeployment(ctx, c, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a")
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultNone {
		t.Fatalf("expected second Deployment reconcile to be a no-op, got %s", op)
	}
}

func TestEnsureRemoteDeploymentRejectsIncompatibleSelector(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelAppName: "other"}},
		},
	})

	if _, _, err := ensureRemoteDeployment(ctx, c, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a"); err == nil {
		t.Fatal("expected incompatible selector to fail")
	}
}

func TestReusableCASecretValidatesStrictPEMShape(t *testing.T) {
	certPEM, keyPEM, err := generateWebhookCACertificate()
	if err != nil {
		t.Fatal(err)
	}

	caCert, caKey, ok := reusableCASecret(webhookCASecretWithData(certPEM, keyPEM))
	if !ok {
		t.Fatal("expected generated CA Secret to be reusable")
	}

	servingCertPEM, _, err := generateWebhookServingCertificate(testWebhookNamespace, caCert, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name    string
		certPEM []byte
		keyPEM  []byte
	}{
		{
			name:    "extra certificate block",
			certPEM: joinPEM(certPEM, certPEM),
			keyPEM:  keyPEM,
		},
		{
			name:    "trailing garbage",
			certPEM: joinPEM(certPEM, []byte("garbage")),
			keyPEM:  keyPEM,
		},
		{
			name:    "non CA certificate",
			certPEM: servingCertPEM,
			keyPEM:  keyPEM,
		},
		{
			name:    "pkcs8 RSA private key",
			certPEM: certPEM,
			keyPEM:  marshalPKCS8PrivateKeyPEM(t, caKey),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, ok := reusableCASecret(webhookCASecretWithData(tt.certPEM, tt.keyPEM)); ok {
				t.Fatal("expected CA Secret not to be reusable")
			}
		})
	}
}

//nolint:funlen // single semantic concern (cert-reuse trust + shape) with shared crypto setup feeding both happy-path and table-driven negatives
func TestReusableServingTLSCertValidatesTrustAndShape(t *testing.T) {
	caCertPEM, caKeyPEM, err := generateWebhookCACertificate()
	if err != nil {
		t.Fatal(err)
	}

	caCert := mustParseSingleCertificatePEM(t, caCertPEM)
	caKey := mustParseRSAPrivateKeyPEM(t, caKeyPEM)

	certPEM, keyPEM, err := generateWebhookServingCertificate(testWebhookNamespace, caCert, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		t.Fatal(err)
	}

	notAfter, fingerprint, ok := reusableServingTLSCert(webhookTLSSecretWithData(certPEM, keyPEM), testWebhookNamespace, caCert)
	if !ok {
		t.Fatal("expected generated serving TLS Secret to be reusable")
	}

	if notAfter.IsZero() || fingerprint != certificateFingerprint(certPEM) {
		t.Fatalf("unexpected reusable serving cert metadata: notAfter=%s fingerprint=%q", notAfter, fingerprint)
	}

	_, mismatchedKeyPEM, err := generateWebhookServingCertificate(testWebhookNamespace, caCert, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		t.Fatal(err)
	}

	wrongCACertPEM, _, err := generateWebhookCACertificate()
	if err != nil {
		t.Fatal(err)
	}

	clientOnlyCertPEM, clientOnlyKeyPEM, err := generateWebhookServingCertificate(testWebhookNamespace, caCert, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name      string
		certPEM   []byte
		keyPEM    []byte
		namespace string
		caCert    *x509.Certificate
	}{
		{
			name:      "certificate bundle",
			certPEM:   joinPEM(certPEM, caCertPEM),
			keyPEM:    keyPEM,
			namespace: testWebhookNamespace,
			caCert:    caCert,
		},
		{
			name:      "wrong namespace",
			certPEM:   certPEM,
			keyPEM:    keyPEM,
			namespace: "other-namespace",
			caCert:    caCert,
		},
		{
			name:      "wrong CA",
			certPEM:   certPEM,
			keyPEM:    keyPEM,
			namespace: testWebhookNamespace,
			caCert:    mustParseSingleCertificatePEM(t, wrongCACertPEM),
		},
		{
			name:      "mismatched key",
			certPEM:   certPEM,
			keyPEM:    mismatchedKeyPEM,
			namespace: testWebhookNamespace,
			caCert:    caCert,
		},
		{
			name:      "missing server auth usage",
			certPEM:   clientOnlyCertPEM,
			keyPEM:    clientOnlyKeyPEM,
			namespace: testWebhookNamespace,
			caCert:    caCert,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, ok := reusableServingTLSCert(webhookTLSSecretWithData(tt.certPEM, tt.keyPEM), tt.namespace, tt.caCert); ok {
				t.Fatal("expected serving TLS Secret not to be reusable")
			}
		})
	}
}

func TestSelfHostedWebhookRuntimeCacheSelectorRequiresRuntimeLabel(t *testing.T) {
	selector := selfHostedWebhookRuntimeCacheSelector()

	if !selector.Matches(labels.Set(webhookRuntimeLabels())) {
		t.Fatalf("expected selector %q to match stamped runtime labels", selector)
	}

	withoutRuntime := labels.Set{
		identityv1.LabelManagedBy: identityv1.ManagedByValue,
		labelAppName:              webhookComponentName,
	}
	if selector.Matches(withoutRuntime) {
		t.Fatalf("expected selector %q to reject labels without %s=%s", selector, identityv1.LabelRuntime, identityv1.RuntimeWebhook)
	}

	unrelated := labels.Set{
		identityv1.LabelManagedBy: identityv1.ManagedByValue,
		labelAppName:              "other-component",
	}
	if selector.Matches(unrelated) {
		t.Fatalf("expected selector %q to reject unrelated operator-managed labels", selector)
	}
}

func TestSelfHostedWebhookRuntimeServiceAccountWatchedButUnscoped(t *testing.T) {
	watchObjects := SelfHostedWebhookRuntimeWatchObjects()
	if !containsObjectType[*corev1.ServiceAccount](watchObjects) {
		t.Fatalf("expected ServiceAccount in watch objects, got %#v", watchObjects)
	}

	scopedObjects := mapKeys(SelfHostedWebhookRuntimeCacheByObject())
	if containsObjectType[*corev1.ServiceAccount](scopedObjects) {
		t.Fatalf("expected ServiceAccount to be absent from cache-scoped objects, got %#v", scopedObjects)
	}
}

type managedWebhookRuntimeObjectCase struct {
	name string
	obj  client.Object
	want bool
}

func TestIsManagedWebhookRuntimeObject(t *testing.T) {
	for _, tt := range managedWebhookRuntimeObjectCases() {
		if got := isManagedWebhookRuntimeObject(tt.obj); got != tt.want {
			t.Fatalf("%s: isManagedWebhookRuntimeObject() = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func managedWebhookRuntimeObjectCases() []managedWebhookRuntimeObjectCase {
	return []managedWebhookRuntimeObjectCase{
		{
			name: "new runtime label",
			obj: webhookRuntimeSecret(webhookComponentName, map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
				identityv1.LabelRuntime:   identityv1.RuntimeWebhook,
			}),
			want: true,
		},
		{
			name: "managed component without runtime label",
			obj: webhookRuntimeSecret(webhookComponentName, map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
				labelAppName:              webhookComponentName,
			}),
		},
		{
			name: "managed ca secret without runtime label",
			obj: webhookRuntimeSecret(webhookCASecretName, map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
				labelAppName:              webhookComponentName,
			}),
		},
		{
			name: "unrelated managed object",
			obj: webhookRuntimeSecret("other", map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
				labelAppName:              webhookComponentName,
			}),
		},
		{
			name: "missing managed by",
			obj: webhookRuntimeSecret(webhookComponentName, map[string]string{
				labelAppName: webhookComponentName,
			}),
		},
	}
}

func webhookRuntimeSecret(name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestWebhookRuntimePredicateKeepsLabelRemovalUpdate(t *testing.T) {
	predicate := webhookRuntimeObjectPredicate()
	oldObj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:   webhookComponentName,
		Labels: webhookRuntimeLabels(),
	}}
	newObj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}

	if !predicate.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj}) {
		t.Fatal("expected runtime predicate to keep update when selector labels are removed from old managed object")
	}
}

//nolint:funlen // table-driven predicate cases kept inline; extracting them obscures the per-case mutate/wantReconcile pairing.
func TestWebhookRuntimePredicateDeploymentUpdates(t *testing.T) {
	predicate := webhookRuntimeObjectPredicate()

	for _, tt := range []struct {
		name          string
		mutate        func(*appsv1.Deployment)
		wantReconcile bool
	}{
		{
			name: "status only update",
			mutate: func(d *appsv1.Deployment) {
				d.ResourceVersion = "2"
				d.ManagedFields = []metav1.ManagedFieldsEntry{{
					Manager:     "deployment-controller",
					Operation:   metav1.ManagedFieldsOperationUpdate,
					APIVersion:  "apps/v1",
					Subresource: "status",
				}}
				d.Status.AvailableReplicas = 1
				d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
			},
			wantReconcile: true,
		},
		{
			name:          "status: ObservedGeneration bump",
			mutate:        func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 1 },
			wantReconcile: true,
		},
		{
			name:          "status: UpdatedReplicas change",
			mutate:        func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 1 },
			wantReconcile: true,
		},
		{
			name: "status: DeploymentAvailable condition flip",
			mutate: func(d *appsv1.Deployment) {
				d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
			},
			wantReconcile: true,
		},
		{
			name:          "generation bump alone",
			mutate:        func(d *appsv1.Deployment) { d.Generation = 1 },
			wantReconcile: true,
		},
		{
			// Pins reliance on Generation, not Spec — apiserver always bumps both together.
			name: "spec change without generation bump",
			mutate: func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Containers[0].Image = testWebhookImageNew
			},
			wantReconcile: false,
		},
		{
			// Top-level annotations are observer drift, not controller-managed.
			name: "annotation change without generation bump",
			mutate: func(d *appsv1.Deployment) {
				d.Annotations = map[string]string{"example.com/drift": "changed"}
			},
			wantReconcile: false,
		},
		{
			name:          "label removal",
			mutate:        func(d *appsv1.Deployment) { d.Labels = nil },
			wantReconcile: true,
		},
		{
			name: "owner reference drift",
			mutate: func(d *appsv1.Deployment) {
				d.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "drift", UID: "drift-uid"}}
			},
			wantReconcile: true,
		},
		{
			name:          "finalizer drift",
			mutate:        func(d *appsv1.Deployment) { d.Finalizers = []string{"example.com/drift"} },
			wantReconcile: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldObj := webhookRuntimeDeploymentForPredicate()
			newObj := oldObj.DeepCopy()
			tt.mutate(newObj)

			if got := predicate.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj}); got != tt.wantReconcile {
				t.Fatalf("predicate.Update() = %v, want %v", got, tt.wantReconcile)
			}
		})
	}
}

func webhookRuntimeDeploymentForPredicate() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookComponentName,
			Namespace: testWebhookNamespace,
			Labels:    webhookRuntimeLabels(),
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "webhook",
						Image: testWebhookImageOld,
					}},
				},
			},
		},
	}
}

func availableWebhookDeployment() *appsv1.Deployment {
	// The apply-side adoption guard refuses to mutate pre-existing Deployments
	// that lack the managed-by/runtime label set, so the available-Status seed
	// used by reconcile/apply tests stamps the same labels the operator would
	// stamp on its own create — i.e. simulates a Deployment created by a prior
	// reconcile of this operator. The Spec.Selector + PodTemplate labels must
	// match webhookDeploymentSelector() because Deployment.Spec.Selector is
	// immutable and ensureRemoteDeployment rejects selector drift.
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       webhookComponentName,
			Namespace:  testWebhookNamespace,
			Generation: 1,
			Labels:     webhookRuntimeLabels(),
		},
		Spec: appsv1.DeploymentSpec{
			Selector: webhookDeploymentSelector(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: webhookDeploymentPodLabels()},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			ReadyReplicas:      1,
			AvailableReplicas:  1,
			Conditions: []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func webhookCASecretWithData(certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		Data: map[string][]byte{
			webhookCACertKey: certPEM,
			webhookCAKeyKey:  keyPEM,
		},
	}
}

func webhookTLSSecretWithData(certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
}

func joinPEM(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func mustParseSingleCertificatePEM(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()

	cert, err := parseSingleCertificatePEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}

	return cert
}

func mustParseRSAPrivateKeyPEM(t *testing.T, keyPEM []byte) *rsa.PrivateKey {
	t.Helper()

	key, err := parseRSAPrivateKeyPEM(keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	return key
}

func marshalPKCS8PrivateKeyPEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: keyutil.PrivateKeyBlockType, Bytes: der})
}

func containsObjectType[T client.Object](objects []client.Object) bool {
	for _, obj := range objects {
		if _, ok := obj.(T); ok {
			return true
		}
	}

	return false
}

func managedWebhookRuntimeObjectMeta(namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      webhookComponentName,
		Namespace: namespace,
		Labels:    webhookRuntimeLabels(),
	}
}

func mapKeys[V any](m map[client.Object]V) []client.Object {
	objects := make([]client.Object, 0, len(m))
	for obj := range m {
		objects = append(objects, obj)
	}

	return objects
}

func testWebhookRuntimeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		admissionregistrationv1.AddToScheme,
		appsv1.AddToScheme,
		corev1.AddToScheme,
		rbacv1.AddToScheme,
		identityv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	return scheme
}

func containerByName(containers []corev1.Container, name string) (*corev1.Container, bool) {
	for i := range containers {
		container := &containers[i]
		if container.Name == name {
			return container, true
		}
	}

	return nil, false
}

func volumeByName(volumes []corev1.Volume, name string) (*corev1.Volume, bool) {
	for i := range volumes {
		volume := &volumes[i]
		if volume.Name == name {
			return volume, true
		}
	}

	return nil, false
}

func assertWebhookNamespaceAndCoreResources(t *testing.T, c client.Client) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal(err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sa), sa); err != nil {
		t.Fatal(err)
	}

	assertWebhookRuntimeLabels(t, ns)
	assertWebhookRuntimeLabels(t, sa)
}

func assertWebhookTLSSecret(t *testing.T, c client.Client) {
	t.Helper()

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}

	if len(secret.Data[corev1.TLSCertKey]) == 0 || len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		t.Fatalf("expected TLS secret data, got %#v", secret.Data)
	}

	if secret.Type != corev1.SecretTypeTLS {
		t.Fatalf("expected TLS Secret type, got %q", secret.Type)
	}

	caSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(caSecret), caSecret); err != nil {
		t.Fatal(err)
	}

	caCert, _, ok := reusableCASecret(caSecret)
	if !ok {
		t.Fatal("expected CA Secret to be reusable")
	}

	if _, _, ok := reusableServingTLSCert(secret, testWebhookNamespace, caCert); !ok {
		t.Fatal("expected TLS Secret to verify against CA Secret")
	}

	assertWebhookRuntimeLabels(t, secret)
}

func assertWebhookCASecret(t *testing.T, c client.Client) {
	t.Helper()

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}

	if len(secret.Data[webhookCACertKey]) == 0 || len(secret.Data[webhookCAKeyKey]) == 0 {
		t.Fatalf("expected CA secret data, got %#v", secret.Data)
	}

	if secret.Type != corev1.SecretTypeOpaque {
		t.Fatalf("expected opaque CA Secret type, got %q", secret.Type)
	}

	if _, _, ok := reusableCASecret(secret); !ok {
		t.Fatal("expected CA Secret to be reusable")
	}

	assertWebhookRuntimeLabels(t, secret)
}

func assertWebhookMutatingWebhookConfiguration(t *testing.T, c client.Client) {
	t.Helper()

	webhook := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(webhook), webhook); err != nil {
		t.Fatal(err)
	}

	if len(webhook.Webhooks) != 1 {
		t.Fatalf("expected one mutating webhook, got %d", len(webhook.Webhooks))
	}

	clientConfig := webhook.Webhooks[0].ClientConfig
	if len(clientConfig.CABundle) == 0 || clientConfig.Service == nil || clientConfig.Service.Namespace != testWebhookNamespace {
		t.Fatalf("unexpected webhook client config: %#v", clientConfig)
	}

	assertWebhookRuntimeLabels(t, webhook)
}

func assertWebhookDeployment(t *testing.T, c client.Client) {
	t.Helper()

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatal(err)
	}

	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected one webhook container, got %d", len(containers))
	}

	if containers[0].Image != testPodIdentityWebhookImage {
		t.Fatalf("unexpected webhook image %q", containers[0].Image)
	}

	assertWebhookPodSecurityContext(t, deploy.Spec.Template.Spec.SecurityContext)
	assertWebhookContainerSecurityContext(t, &containers[0])

	if len(containers[0].Command) != 1 || containers[0].Command[0] != "/webhook" {
		t.Fatalf("unexpected webhook command %#v", containers[0].Command)
	}

	if len(containers[0].Args) == 0 || containers[0].Args[0] != "--in-cluster=false" {
		t.Fatalf("expected out-of-cluster certificate mode, got args %#v", containers[0].Args)
	}

	gotFingerprint := deploy.Spec.Template.Annotations[webhookServingCertFingerprintKey]
	if gotFingerprint == "" {
		t.Fatalf("expected serving cert fingerprint annotation, got %#v", deploy.Spec.Template.Annotations)
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}

	if want := certificateFingerprint(secret.Data[corev1.TLSCertKey]); gotFingerprint != want {
		t.Fatalf("expected serving cert fingerprint annotation %q, got %q", want, gotFingerprint)
	}

	assertWebhookRuntimeLabels(t, deploy)
}

func assertWebhookPodSecurityContext(t *testing.T, securityContext *corev1.PodSecurityContext) {
	t.Helper()

	if securityContext == nil || securityContext.SeccompProfile == nil || securityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("expected RuntimeDefault pod seccomp profile, got %#v", securityContext)
	}
}

func assertWebhookContainerSecurityContext(t *testing.T, container *corev1.Container) {
	t.Helper()

	securityContext := container.SecurityContext
	if securityContext == nil {
		t.Fatalf("expected webhook container security context, got %#v", container)
	}

	if securityContext.AllowPrivilegeEscalation == nil || *securityContext.AllowPrivilegeEscalation {
		t.Fatalf("expected allowPrivilegeEscalation=false, got %#v", securityContext.AllowPrivilegeEscalation)
	}

	if securityContext.RunAsNonRoot == nil || !*securityContext.RunAsNonRoot {
		t.Fatalf("expected runAsNonRoot=true, got %#v", securityContext.RunAsNonRoot)
	}

	if securityContext.RunAsUser == nil || *securityContext.RunAsUser != webhookRuntimeUserID {
		t.Fatalf("expected runAsUser=%d, got %#v", webhookRuntimeUserID, securityContext.RunAsUser)
	}

	if securityContext.RunAsGroup == nil || *securityContext.RunAsGroup != webhookRuntimeUserID {
		t.Fatalf("expected runAsGroup=%d, got %#v", webhookRuntimeUserID, securityContext.RunAsGroup)
	}

	if securityContext.Capabilities == nil ||
		!reflect.DeepEqual(securityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		!reflect.DeepEqual(securityContext.Capabilities.Add, []corev1.Capability{"NET_BIND_SERVICE"}) {
		t.Fatalf("unexpected webhook container capabilities: %#v", securityContext.Capabilities)
	}

	if securityContext.SeccompProfile == nil || securityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("expected RuntimeDefault container seccomp profile, got %#v", securityContext.SeccompProfile)
	}
}

func assertWebhookPodTemplateExcludedFromMutation(t *testing.T, c client.Client) {
	t.Helper()

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatal(err)
	}

	if got := deploy.Spec.Template.Labels[selfHostedSkipWebhookLabel]; got != webhookSkipLabelValue {
		t.Fatalf("expected webhook pod template to set skip label %q=true, got %q in %#v", selfHostedSkipWebhookLabel, got, deploy.Spec.Template.Labels)
	}

	webhook := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(webhook), webhook); err != nil {
		t.Fatal(err)
	}

	if len(webhook.Webhooks) != 1 {
		t.Fatalf("expected one mutating webhook, got %d", len(webhook.Webhooks))
	}

	objectSelector := webhook.Webhooks[0].ObjectSelector
	if objectSelector == nil {
		t.Fatal("expected mutating webhook objectSelector")
	}

	for _, requirement := range objectSelector.MatchExpressions {
		if requirement.Key == selfHostedSkipWebhookLabel && requirement.Operator == metav1.LabelSelectorOpDoesNotExist {
			return
		}
	}

	t.Fatalf("expected objectSelector to skip pods with %q, got %#v", selfHostedSkipWebhookLabel, objectSelector.MatchExpressions)
}

func assertWebhookRuntimeLabels(t *testing.T, obj client.Object) {
	t.Helper()

	labels := obj.GetLabels()
	if labels[identityv1.LabelManagedBy] != identityv1.ManagedByValue ||
		labels[identityv1.LabelRuntime] != identityv1.RuntimeWebhook ||
		labels[identityv1.LabelDelivery] != string(identityv1.DeliveryTypeSelfHostedIRSA) ||
		labels[labelAppName] != webhookComponentName {
		t.Fatalf("unexpected runtime labels on %T %s/%s: %#v", obj, obj.GetNamespace(), obj.GetName(), labels)
	}
}

// TestEnsureRemoteClusterRoleServiceAccountsOnly pins that the remote
// webhook's ClusterRole grants exactly one PolicyRule (serviceaccounts
// get/list/watch) and intentionally omits the CSR verbs that belong to
// --in-cluster=true mode. The negative assertions guard against regressions
// re-introducing `certificatesigningrequests` or any `secrets` access.
func TestEnsureRemoteClusterRoleServiceAccountsOnly(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t)

	op, err := ensureRemoteClusterRole(ctx, c, c, testWebhookNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultCreated {
		t.Fatalf("expected ClusterRole to be created on first apply, got operation %s", op)
	}

	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(clusterRole), clusterRole); err != nil {
		t.Fatal(err)
	}

	if len(clusterRole.Rules) != 1 {
		t.Fatalf("expected exactly one PolicyRule, got %d: %#v", len(clusterRole.Rules), clusterRole.Rules)
	}

	rule := clusterRole.Rules[0]
	if !reflect.DeepEqual(rule.APIGroups, []string{""}) {
		t.Fatalf("expected APIGroups=[\"\"], got %#v", rule.APIGroups)
	}

	if !reflect.DeepEqual(rule.Resources, []string{"serviceaccounts"}) {
		t.Fatalf("expected Resources=[\"serviceaccounts\"], got %#v", rule.Resources)
	}

	if !reflect.DeepEqual(rule.Verbs, []string{"get", "list", "watch"}) {
		t.Fatalf("expected Verbs=[\"get\",\"list\",\"watch\"], got %#v", rule.Verbs)
	}

	for _, r := range clusterRole.Rules {
		for _, res := range r.Resources {
			if res == "certificatesigningrequests" {
				t.Fatalf("expected ClusterRole to omit certificatesigningrequests, got rule %#v", r)
			}

			if res == "secrets" {
				t.Fatalf("expected ClusterRole to omit secrets, got rule %#v", r)
			}
		}

		for _, g := range r.APIGroups {
			if g == "certificates.k8s.io" {
				t.Fatalf("expected ClusterRole to omit certificates.k8s.io APIGroup, got rule %#v", r)
			}
		}
	}

	assertWebhookRuntimeLabels(t, clusterRole)
}

func TestDesiredWebhookContainerResources(t *testing.T) {
	c := desiredWebhookContainer(testWebhookNamespace, testPodIdentityWebhookImage)

	wantCPURequest := resource.MustParse("50m")
	if got, ok := c.Resources.Requests[corev1.ResourceCPU]; !ok || got.Cmp(wantCPURequest) != 0 {
		t.Fatalf("expected Resources.Requests[cpu]=50m, got %v (present=%t)", got.String(), ok)
	}

	wantMemRequest := resource.MustParse("64Mi")
	if got, ok := c.Resources.Requests[corev1.ResourceMemory]; !ok || got.Cmp(wantMemRequest) != 0 {
		t.Fatalf("expected Resources.Requests[memory]=64Mi, got %v (present=%t)", got.String(), ok)
	}

	wantMemLimit := resource.MustParse("128Mi")
	if got, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok || got.Cmp(wantMemLimit) != 0 {
		t.Fatalf("expected Resources.Limits[memory]=128Mi, got %v (present=%t)", got.String(), ok)
	}

	if got, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Fatalf("expected Resources.Limits to omit cpu, got %s", got.String())
	}
}

func TestDesiredWebhookContainerLivenessProbe(t *testing.T) {
	c := desiredWebhookContainer(testWebhookNamespace, testPodIdentityWebhookImage)
	httpsPort := intstr.FromString("https")

	if c.LivenessProbe == nil {
		t.Fatal("expected LivenessProbe to be set")
	}

	if c.LivenessProbe.HTTPGet == nil {
		t.Fatal("expected LivenessProbe.HTTPGet to be set")
	}

	if c.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("expected LivenessProbe path=/healthz, got %q", c.LivenessProbe.HTTPGet.Path)
	}

	if c.LivenessProbe.HTTPGet.Port != httpsPort {
		t.Fatalf("expected LivenessProbe port=https, got %v", c.LivenessProbe.HTTPGet.Port)
	}

	if c.LivenessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Fatalf("expected LivenessProbe scheme=HTTPS, got %v", c.LivenessProbe.HTTPGet.Scheme)
	}

	if c.LivenessProbe.InitialDelaySeconds != 10 {
		t.Fatalf("expected LivenessProbe InitialDelaySeconds=10, got %d", c.LivenessProbe.InitialDelaySeconds)
	}

	if c.LivenessProbe.PeriodSeconds != 20 {
		t.Fatalf("expected LivenessProbe PeriodSeconds=20, got %d", c.LivenessProbe.PeriodSeconds)
	}
}

func TestDesiredWebhookContainerReadinessProbe(t *testing.T) {
	c := desiredWebhookContainer(testWebhookNamespace, testPodIdentityWebhookImage)
	httpsPort := intstr.FromString("https")

	if c.ReadinessProbe == nil {
		t.Fatal("expected ReadinessProbe to be set")
	}

	if c.ReadinessProbe.HTTPGet == nil {
		t.Fatal("expected ReadinessProbe.HTTPGet to be set")
	}

	if c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("expected ReadinessProbe path=/healthz, got %q", c.ReadinessProbe.HTTPGet.Path)
	}

	if c.ReadinessProbe.HTTPGet.Port != httpsPort {
		t.Fatalf("expected ReadinessProbe port=https, got %v", c.ReadinessProbe.HTTPGet.Port)
	}

	if c.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		t.Fatalf("expected ReadinessProbe scheme=HTTPS, got %v", c.ReadinessProbe.HTTPGet.Scheme)
	}

	if c.ReadinessProbe.InitialDelaySeconds != 5 {
		t.Fatalf("expected ReadinessProbe InitialDelaySeconds=5, got %d", c.ReadinessProbe.InitialDelaySeconds)
	}

	if c.ReadinessProbe.PeriodSeconds != 10 {
		t.Fatalf("expected ReadinessProbe PeriodSeconds=10, got %d", c.ReadinessProbe.PeriodSeconds)
	}
}

// assessRemoteDeploymentReadiness gates caBundle rotation against the webhook
// Deployment's rollout status and surfaces a per-stage Reason/Message for
// status conditions. Regressions here can rotate the TLS trust material while
// old-revision Pods are still serving traffic, breaking the admission webhook.
// The cases below pin the contract:
//
//   - Status.ObservedGeneration must have caught up to Generation (stale
//     status reject — the stale-status rotation regression this test was added for).
//   - Status.UpdatedReplicas must meet Spec.Replicas (rollout complete; raw
//     AvailableReplicas can otherwise count old-revision pods).
//   - Status.AvailableReplicas must meet Spec.Replicas.
//   - Conditions[Available] must be True.
//   - Spec.Replicas == nil defaults to 1 (Deployment API default).
//
//nolint:funlen // table-driven rollout cases kept inline; extracting them obscures the per-case status/want pairing.
func TestAssessRemoteDeploymentReadiness(t *testing.T) {
	availableCondition := func(status corev1.ConditionStatus) []appsv1.DeploymentCondition {
		return []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentAvailable,
			Status: status,
		}}
	}

	tests := []struct {
		name                string
		deployment          *appsv1.Deployment
		wantReady           bool
		wantReason          string
		wantMessageContains string
	}{
		{
			name: "Available",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           1,
					UpdatedReplicas:    1,
					AvailableReplicas:  1,
					Conditions:         availableCondition(corev1.ConditionTrue),
				},
			},
			wantReady: true,
		},
		{
			// Stale status: Deployment controller has not yet observed the
			// current Spec, so AvailableReplicas refers to the previous
			// revision. Trusting it would rotate caBundle against old Pods.
			name: "StaleObservedGeneration",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           1,
					UpdatedReplicas:    1,
					AvailableReplicas:  1,
					Conditions:         availableCondition(corev1.ConditionTrue),
				},
			},
			wantReady:           false,
			wantReason:          identityv1.ReasonWebhookDeploymentObservedGenerationLag,
			wantMessageContains: "observedGeneration=1, generation=2",
		},
		{
			// Mid-rollout: AvailableReplicas counts old-revision Pods, but
			// only one new Pod has rolled. Must wait until UpdatedReplicas
			// catches up to desired.
			name: "UpdatedReplicasInsufficient",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           2,
					UpdatedReplicas:    1,
					AvailableReplicas:  2,
					Conditions:         availableCondition(corev1.ConditionTrue),
				},
			},
			wantReady:           false,
			wantReason:          identityv1.ReasonWebhookDeploymentRolloutInProgress,
			wantMessageContains: "updatedReplicas=1, desired=2",
		},
		{
			name: "AvailableReplicasInsufficient",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           2,
					UpdatedReplicas:    2,
					AvailableReplicas:  1,
					Conditions:         availableCondition(corev1.ConditionTrue),
				},
			},
			wantReady:           false,
			wantReason:          identityv1.ReasonWebhookDeploymentReplicasUnavailable,
			wantMessageContains: "availableReplicas=1, desired=2",
		},
		{
			name: "ConditionAvailableFalse",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           1,
					UpdatedReplicas:    1,
					AvailableReplicas:  1,
					Conditions:         availableCondition(corev1.ConditionFalse),
				},
			},
			wantReady:           false,
			wantReason:          identityv1.ReasonWaitingForWebhookDeployment,
			wantMessageContains: "Available condition is not True",
		},
		{
			name: "ConditionAvailableMissing",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           1,
					UpdatedReplicas:    1,
					AvailableReplicas:  1,
				},
			},
			wantReady:           false,
			wantReason:          identityv1.ReasonWaitingForWebhookDeployment,
			wantMessageContains: "Available condition is not True",
		},
		{
			// Deployment API default: nil Replicas means 1.
			name: "DefaultReplicaWhenSpecNil",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: nil},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           1,
					UpdatedReplicas:    1,
					AvailableReplicas:  1,
					Conditions:         availableCondition(corev1.ConditionTrue),
				},
			},
			wantReady: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := assessRemoteDeploymentReadiness(tc.deployment)
			if got.Ready != tc.wantReady {
				t.Errorf("assessRemoteDeploymentReadiness().Ready = %t, want %t", got.Ready, tc.wantReady)
			}

			if tc.wantReady {
				return
			}

			if got.Reason != tc.wantReason {
				t.Errorf("assessRemoteDeploymentReadiness().Reason = %q, want %q", got.Reason, tc.wantReason)
			}

			if !strings.Contains(got.Message, tc.wantMessageContains) {
				t.Errorf("assessRemoteDeploymentReadiness().Message = %q, want substring %q", got.Message, tc.wantMessageContains)
			}
		})
	}
}
