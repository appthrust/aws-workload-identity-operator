package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestLocalSecretCacheByObjectReturnsSecretEntry(t *testing.T) {
	byObject := LocalSecretCacheByObject()

	var (
		entry client.Object
		found bool
	)

	for obj := range byObject {
		if _, ok := obj.(*corev1.Secret); ok {
			entry = obj
			found = true

			break
		}
	}

	if !found {
		t.Fatalf("expected Secret ByObject entry, got %#v", byObject)
	}

	cfg := byObject[entry]
	if cfg.Transform == nil {
		t.Fatal("expected Secret cache Transform to be set")
	}

	if cfg.Namespaces == nil || len(cfg.Namespaces) != 0 {
		t.Fatalf("expected Secret cache to explicitly watch all namespaces, got %#v", cfg.Namespaces)
	}
}

func TestLocalNamespaceCacheByObjectReturnsNamespaceTransform(t *testing.T) {
	byObject := LocalNamespaceCacheByObject()

	var (
		entry client.Object
		found bool
	)

	for obj := range byObject {
		if _, ok := obj.(*corev1.Namespace); ok {
			entry = obj
			found = true

			break
		}
	}

	if !found {
		t.Fatalf("expected Namespace ByObject entry, got %#v", byObject)
	}

	cfg := byObject[entry]
	if cfg.Transform == nil {
		t.Fatal("expected Namespace cache Transform to be set to strip managed fields")
	}
}

func TestLocalNamespaceCacheTransformStripsManagedFields(t *testing.T) {
	byObject := LocalNamespaceCacheByObject()

	var transform toolscache.TransformFunc
	for _, cfg := range byObject {
		transform = cfg.Transform

		break
	}

	if transform == nil {
		t.Fatal("expected Namespace cache Transform to be configured")
	}

	in := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "tenant-a",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}, {Manager: "kube-controller-manager"}},
		},
	}

	out, err := transform(in)
	if err != nil {
		t.Fatal(err)
	}

	ns, ok := out.(*corev1.Namespace)
	if !ok {
		t.Fatalf("expected *corev1.Namespace, got %T", out)
	}

	if len(ns.ManagedFields) != 0 {
		t.Fatalf("expected ManagedFields to be stripped, got %#v", ns.ManagedFields)
	}

	if ns.Name != "tenant-a" {
		t.Fatalf("expected Name to be preserved, got %q", ns.Name)
	}
}

func TestLocalSecretCacheTransformStripsForeignSecretData(t *testing.T) {
	transform := stripForeignSecretDataForCache()

	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-secret",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "some-other-controller",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("super-secret")},
		StringData: map[string]string{"alt": "also-secret"},
	}

	out, err := transform(foreign)
	if err != nil {
		t.Fatal(err)
	}

	secret, ok := out.(*corev1.Secret)
	if !ok {
		t.Fatalf("expected *corev1.Secret, got %T", out)
	}

	if secret.Data != nil {
		t.Fatalf("expected foreign Secret Data to be stripped, got %#v", secret.Data)
	}

	if secret.StringData != nil {
		t.Fatalf("expected foreign Secret StringData to be stripped, got %#v", secret.StringData)
	}

	if len(secret.ManagedFields) != 0 {
		t.Fatalf("expected ManagedFields to be stripped, got %#v", secret.ManagedFields)
	}

	if secret.Name != "user-secret" || secret.Namespace != "default" {
		t.Fatalf("expected metadata to be preserved, got %s/%s", secret.Namespace, secret.Name)
	}

	if secret.Type != corev1.SecretTypeOpaque {
		t.Fatalf("expected Type to be preserved, got %q", secret.Type)
	}
}

func TestLocalSecretCacheTransformPreservesManagedSecretData(t *testing.T) {
	transform := stripForeignSecretDataForCache()

	owner := metav1.OwnerReference{
		APIVersion: identityv1.GroupVersion.String(),
		Kind:       "AWSWorkloadIdentityConfig",
		Name:       "default",
		UID:        "uid-1",
	}

	managed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "signing-key-default",
			Namespace: "ns-1",
			Labels: map[string]string{
				identityv1.LabelManagedBy: identityv1.ManagedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{owner},
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "manager"}},
		},
		Data: map[string][]byte{
			"private.pem": []byte("PRIVATE"),
			"public.pem":  []byte("PUBLIC"),
		},
	}

	out, err := transform(managed)
	if err != nil {
		t.Fatal(err)
	}

	secret, ok := out.(*corev1.Secret)
	if !ok {
		t.Fatalf("expected *corev1.Secret, got %T", out)
	}

	if string(secret.Data["private.pem"]) != "PRIVATE" || string(secret.Data["public.pem"]) != "PUBLIC" {
		t.Fatalf("expected managed Secret Data to be preserved, got %#v", secret.Data)
	}

	if len(secret.ManagedFields) != 0 {
		t.Fatalf("expected ManagedFields to be stripped even on managed Secrets, got %#v", secret.ManagedFields)
	}

	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != "uid-1" {
		t.Fatalf("expected OwnerReferences to be preserved, got %#v", secret.OwnerReferences)
	}
}

func TestLocalSecretCacheTransformPreservesDataWhenOnlyConfigUIDLabelRemains(t *testing.T) {
	transform := stripForeignSecretDataForCache()

	managed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "signing-key-default",
			Namespace: "ns-1",
			Labels: map[string]string{
				// LabelManagedBy is missing (e.g. removed by kubectl label),
				// but the operator-domain-scoped config-uid label still pins
				// the Secret to a specific config.
				identityv1.LabelConfigUID: "uid-1",
			},
		},
		Data: map[string][]byte{
			"private.pem": []byte("PRIVATE"),
		},
	}

	out, err := transform(managed)
	if err != nil {
		t.Fatal(err)
	}

	secret, ok := out.(*corev1.Secret)
	if !ok {
		t.Fatalf("expected *corev1.Secret, got %T", out)
	}

	if string(secret.Data["private.pem"]) != "PRIVATE" {
		t.Fatalf("expected Data to be preserved while LabelConfigUID is set, got %#v", secret.Data)
	}
}

func TestLocalSecretCacheTransformIgnoresNonSecretObjects(t *testing.T) {
	transform := stripForeignSecretDataForCache()

	in := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "cm",
			Namespace:     "default",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Data: map[string]string{"key": "value"},
	}

	out, err := transform(in)
	if err != nil {
		t.Fatal(err)
	}

	cm, ok := out.(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("expected *corev1.ConfigMap, got %T", out)
	}

	if cm.Data["key"] != "value" {
		t.Fatalf("expected ConfigMap data to be untouched, got %#v", cm.Data)
	}

	if len(cm.ManagedFields) != 0 {
		t.Fatalf("expected ManagedFields to be stripped by underlying TransformStripManagedFields, got %#v", cm.ManagedFields)
	}
}
