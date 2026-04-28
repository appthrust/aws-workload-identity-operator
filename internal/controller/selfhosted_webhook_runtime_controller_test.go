package controller

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"maps"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/keyutil"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
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

	result, err := applyRemoteWebhookRuntime(context.Background(), logr.Discard(), c, testWebhookNamespace, testPodIdentityWebhookImage)
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

	if _, err := applyRemoteWebhookRuntime(ctx, logr.Discard(), c, testWebhookNamespace, testPodIdentityWebhookImage); err != nil {
		t.Fatal(err)
	}

	first := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(first), first); err != nil {
		t.Fatal(err)
	}

	firstLabels := maps.Clone(first.Labels)
	firstPodLabels := maps.Clone(first.Spec.Template.Labels)
	firstPodAnnotations := maps.Clone(first.Spec.Template.Annotations)

	if _, err := applyRemoteWebhookRuntime(ctx, logr.Discard(), c, testWebhookNamespace, testPodIdentityWebhookImage); err != nil {
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

func TestApplyRemoteWebhookRuntimeRemovesLegacyConfigSpecificLabels(t *testing.T) {
	ctx := context.Background()
	deployment := availableWebhookDeployment()
	deployment.Labels = map[string]string{
		identityv1.LabelConfigUID:   "old-config",
		identityv1.LabelInventoryNS: "old-namespace",
		testWebhookKeepLabelKey:     testWebhookKeepLabelMetadata,
	}
	deployment.Spec.Template.Labels = map[string]string{
		identityv1.LabelConfigUID:   "old-config",
		identityv1.LabelInventoryNS: "old-namespace",
		testWebhookKeepLabelKey:     testWebhookKeepLabelTemplate,
	}
	c := fakeClient(t, deployment)

	if _, err := applyRemoteWebhookRuntime(ctx, logr.Discard(), c, testWebhookNamespace, testPodIdentityWebhookImage); err != nil {
		t.Fatal(err)
	}

	stored := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	assertNoConfigSpecificRuntimeLabels(t, stored.Labels)
	assertNoConfigSpecificRuntimeLabels(t, stored.Spec.Template.Labels)

	if stored.Labels[testWebhookKeepLabelKey] != testWebhookKeepLabelMetadata ||
		stored.Spec.Template.Labels[testWebhookKeepLabelKey] != testWebhookKeepLabelTemplate {
		t.Fatalf("expected unrelated labels to be preserved, metadata=%#v template=%#v", stored.Labels, stored.Spec.Template.Labels)
	}
}

func TestApplyRemoteWebhookRuntimeWaitsForDeploymentBeforeCreatingWebhookConfiguration(t *testing.T) {
	c := fakeClient(t)

	result, err := applyRemoteWebhookRuntime(context.Background(), logr.Discard(), c, testWebhookNamespace, testPodIdentityWebhookImage)
	if err != nil {
		t.Fatal(err)
	}

	if result.Condition.Ready || result.Condition.Reason != identityv1.ReasonWaitingForWebhookDeployment {
		t.Fatalf("expected waiting-for-deployment result, got ready=%t reason=%s", result.Condition.Ready, result.Condition.Reason)
	}

	webhook := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(webhook), webhook); err == nil {
		t.Fatal("expected MutatingWebhookConfiguration not to be created before Deployment is Available")
	}
}

func TestEnsureRemoteNamespaceDoesNotAdoptExistingNamespace(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   testWebhookNamespace,
		Labels: map[string]string{"example.com/owner": "user"},
	}})

	op, err := ensureRemoteNamespace(ctx, c, testWebhookNamespace)
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

func TestEnsureRemoteNamespaceRemovesLegacyConfigSpecificLabels(t *testing.T) {
	ctx := context.Background()
	legacyLabels := webhookRuntimeLabels()
	legacyLabels[identityv1.LabelConfigUID] = "old-config"
	legacyLabels[identityv1.LabelInventoryNS] = "old-namespace"
	legacyLabels[testWebhookKeepLabelKey] = testWebhookKeepLabelMetadata
	c := fakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   testWebhookNamespace,
		Labels: legacyLabels,
	}})

	op, err := ensureRemoteNamespace(ctx, c, testWebhookNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultUpdated {
		t.Fatalf("expected managed legacy Namespace labels to be updated, got operation %s", op)
	}

	stored := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testWebhookNamespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	assertNoConfigSpecificRuntimeLabels(t, stored.Labels)

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
	legacyLabels := webhookRuntimeLabels()
	legacyLabels[identityv1.LabelConfigUID] = "old-config"
	seed := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   testWebhookNamespace,
		Labels: legacyLabels,
	}}

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

	op, err := ensureRemoteNamespace(ctx, c, testWebhookNamespace)
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

	if _, ok := stored.Labels[identityv1.LabelConfigUID]; ok {
		t.Fatalf("expected legacy Config-specific label to be stripped through retry loop, got %#v", stored.Labels)
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
		&rbacv1.ClusterRole{ObjectMeta: managedWebhookRuntimeObjectMeta("")},
	)

	if err := deleteRemoteWebhookRuntime(ctx, c, testWebhookNamespace); err != nil {
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

	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(clusterRole), clusterRole); !apierrors.IsNotFound(err) {
		t.Fatalf("expected managed ClusterRole to be deleted, got %v", err)
	}
}

func TestDeleteRemoteWebhookRuntimeSkipsUnmanagedNamedObjects(t *testing.T) {
	ctx := context.Background()
	unmanagedLabels := map[string]string{"example.com/owner": "user"}
	objects := []client.Object{
		&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace, Labels: maps.Clone(unmanagedLabels)}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Labels: maps.Clone(unmanagedLabels)}},
	}
	c := fakeClient(t, objects...)

	if err := deleteRemoteWebhookRuntime(ctx, c, testWebhookNamespace); err != nil {
		t.Fatal(err)
	}

	for _, obj := range objects {
		key := client.ObjectKeyFromObject(obj)
		if err := c.Get(ctx, key, obj); err != nil {
			t.Fatalf("expected unmanaged %T %s to be preserved: %v", obj, key, err)
		}
	}
}

func TestEnsureRemoteDeploymentPreservesUnmanagedTemplateFields(t *testing.T) {
	ctx := context.Background()

	c := fakeClient(t, webhookDeploymentWithUnmanagedFields())

	_, op, err := ensureRemoteDeployment(ctx, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a")
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
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookComponentName,
			Namespace: testWebhookNamespace,
			Labels: map[string]string{
				"example.com/deployment": testPreservedValue,
			},
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

	if _, _, err := ensureRemoteDeployment(ctx, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a"); err != nil {
		t.Fatal(err)
	}

	_, op, err := ensureRemoteDeployment(ctx, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a")
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

	if _, _, err := ensureRemoteDeployment(ctx, c, testWebhookNamespace, testWebhookImageNew, "fingerprint-a"); err == nil {
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

func TestSelfHostedWebhookRuntimeCacheSelectorMatchesRuntimeAndLegacyLabels(t *testing.T) {
	selector := selfHostedWebhookRuntimeCacheSelector()

	if !selector.Matches(labels.Set(webhookRuntimeLabels())) {
		t.Fatalf("expected selector %q to match stamped runtime labels", selector)
	}

	legacy := labels.Set{
		identityv1.LabelManagedBy: identityv1.ManagedByValue,
		labelAppName:              webhookComponentName,
	}
	if !selector.Matches(legacy) {
		t.Fatalf("expected selector %q to match legacy webhook labels", selector)
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

	uncachedObjects := SelfHostedWebhookRuntimeUncachedReadObjects()
	if containsObjectType[*corev1.ServiceAccount](uncachedObjects) {
		t.Fatalf("expected ServiceAccount to be absent from uncached read objects, got %#v", uncachedObjects)
	}
}

func TestSelfHostedWebhookRuntimeUncachedReadObjectsMatchScopedCacheGVKs(t *testing.T) {
	scheme := testWebhookRuntimeScheme(t)
	cacheGVKs := objectGVKSet(t, scheme, mapKeys(SelfHostedWebhookRuntimeCacheByObject())...)
	uncachedGVKs := objectGVKSet(t, scheme, SelfHostedWebhookRuntimeUncachedReadObjects()...)

	if len(cacheGVKs) != len(uncachedGVKs) {
		t.Fatalf("expected matching GVK counts, cache=%#v uncached=%#v", cacheGVKs, uncachedGVKs)
	}

	for gvk := range cacheGVKs {
		if _, ok := uncachedGVKs[gvk]; !ok {
			t.Fatalf("expected uncached reads to include scoped GVK %s; uncached=%#v", gvk, uncachedGVKs)
		}
	}
}

func TestScopedRuntimeObjectsUseLiveReadsForLegacyAdoption(t *testing.T) {
	ctx := context.Background()
	scheme := testWebhookRuntimeScheme(t)
	legacyCASecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      webhookCASecretName,
		Namespace: testWebhookNamespace,
	}}
	scopedGVKs := objectGVKSet(t, scheme, mapKeys(SelfHostedWebhookRuntimeCacheByObject())...)

	hiddenClient := &cacheScopedReadClient{
		Client:     fakeClient(t, legacyCASecret.DeepCopy()),
		scheme:     scheme,
		hiddenGVKs: scopedGVKs,
	}
	if _, _, _, _, err := ensureRemoteCASecret(ctx, hiddenClient, testWebhookNamespace); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected hidden cached read to recreate legacy Secret and hit AlreadyExists, got %v", err)
	}

	liveReadClient := &cacheScopedReadClient{
		Client:       fakeClient(t, legacyCASecret.DeepCopy()),
		scheme:       scheme,
		hiddenGVKs:   scopedGVKs,
		uncachedGVKs: objectGVKSet(t, scheme, SelfHostedWebhookRuntimeUncachedReadObjects()...),
	}

	op, caBundle, caCert, caKey, err := ensureRemoteCASecret(ctx, liveReadClient, testWebhookNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if op != controllerutil.OperationResultUpdated {
		t.Fatalf("expected live read to update legacy Secret, got operation %s", op)
	}

	if len(caBundle) == 0 || caCert == nil || caKey == nil {
		t.Fatal("expected live read to return CA material")
	}

	if liveReadClient.createCalls != 0 {
		t.Fatalf("expected live read adoption not to create, got %d create calls", liveReadClient.createCalls)
	}

	stored := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: testWebhookNamespace}}
	if err := liveReadClient.Client.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}

	assertWebhookRuntimeLabels(t, stored)

	if len(stored.Data[webhookCACertKey]) == 0 || len(stored.Data[webhookCAKeyKey]) == 0 {
		t.Fatalf("expected legacy CA Secret to be populated, got %#v", stored.Data)
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
			name: "legacy component labels",
			obj: webhookRuntimeSecret(webhookComponentName, map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
				labelAppName:              webhookComponentName,
			}),
			want: true,
		},
		{
			name: "legacy ca secret labels",
			obj: webhookRuntimeSecret(webhookCASecretName, map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
				labelAppName:              webhookComponentName,
			}),
			want: true,
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
		Name: webhookComponentName,
		Labels: map[string]string{
			identityv1.LabelManagedBy: identityv1.ManagedByValue,
			labelAppName:              webhookComponentName,
		},
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
			wantReconcile: false,
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
			Labels: map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
				labelAppName:              webhookComponentName,
			},
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
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: testWebhookNamespace},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: 1,
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
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	return scheme
}

func objectGVKSet(t *testing.T, scheme *runtime.Scheme, objects ...client.Object) map[schema.GroupVersionKind]struct{} {
	t.Helper()

	gvks := make(map[schema.GroupVersionKind]struct{}, len(objects))
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			t.Fatal(err)
		}

		gvks[gvk] = struct{}{}
	}

	return gvks
}

type cacheScopedReadClient struct {
	client.Client

	scheme       *runtime.Scheme
	hiddenGVKs   map[schema.GroupVersionKind]struct{}
	uncachedGVKs map[schema.GroupVersionKind]struct{}
	createCalls  int
}

func (c *cacheScopedReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return fmt.Errorf("resolve GVK for %T: %w", obj, err)
	}

	_, hidden := c.hiddenGVKs[gvk]
	if _, uncached := c.uncachedGVKs[gvk]; hidden && !uncached {
		return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, key.Name)
	}

	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return fmt.Errorf("get %T %s/%s: %w", obj, key.Namespace, key.Name, err)
	}

	return nil
}

func (c *cacheScopedReadClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.createCalls++

	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return fmt.Errorf("create %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
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

	if labels[identityv1.LabelConfigUID] != "" || labels[identityv1.LabelInventoryNS] != "" {
		t.Fatalf("expected shared runtime object to omit Config-specific labels, got %#v", labels)
	}
}

func assertNoConfigSpecificRuntimeLabels(t *testing.T, labels map[string]string) {
	t.Helper()

	if labels[identityv1.LabelConfigUID] != "" || labels[identityv1.LabelInventoryNS] != "" {
		t.Fatalf("expected labels to omit Config-specific runtime keys, got %#v", labels)
	}
}
