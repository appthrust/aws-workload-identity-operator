package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

func TestMetricsServerOptionsSetsAuthFilterProvider(t *testing.T) {
	opts := metricsServerOptions(&managerOptions{metricsAddr: ":8080"})

	if opts.BindAddress != ":8080" {
		t.Fatalf("expected BindAddress %q, got %q", ":8080", opts.BindAddress)
	}

	if opts.FilterProvider == nil {
		t.Fatal("expected FilterProvider to be configured so /metrics requires authn/authz; nil leaves the endpoint anonymous")
	}
}

func TestMetricsServerOptionsHonorsBindAddressZero(t *testing.T) {
	opts := metricsServerOptions(&managerOptions{metricsAddr: "0"})

	if opts.BindAddress != "0" {
		t.Fatalf("expected BindAddress %q (disabled), got %q", "0", opts.BindAddress)
	}

	if opts.FilterProvider == nil {
		t.Fatal("expected FilterProvider to remain configured even when metrics endpoint is disabled by flag")
	}
}

func TestParseFlagsLeaderElectDefaultsToFalse(t *testing.T) {
	oldCommandLine := flag.CommandLine
	oldArgs := os.Args

	t.Cleanup(func() {
		flag.CommandLine = oldCommandLine
		os.Args = oldArgs
	})

	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	os.Args = []string{"test"}

	opts := parseFlags()

	if opts.leaderElect {
		t.Fatal("expected leader-elect default to be false so that Helm chart leaderElection.enabled=false actually disables leader election; binary default must match controller-runtime opt-in convention")
	}
}

func TestValidateOptionsAcceptsHTTPSAWSEndpointURL(t *testing.T) {
	opts := &managerOptions{awsEndpointURL: "https://aws-endpoint.example.com"}

	if err := validateOptions(opts); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOptionsAllowsHTTPOnlyWhenExplicit(t *testing.T) {
	opts := &managerOptions{awsEndpointURL: "http://aws-endpoint.example.com"}

	if err := validateOptions(opts); err == nil {
		t.Fatal("expected unsafe endpoint error")
	}

	opts.allowUnsafeAWSEndpointURL = true
	if err := validateOptions(opts); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOptionsRejectsInvalidAWSEndpointURL(t *testing.T) {
	tests := []string{
		"aws-endpoint.example.com:9000",
		"https://:443",
		"https:/bad",
		"ftp://aws-endpoint.example.com",
	}

	for _, endpoint := range tests {
		opts := &managerOptions{
			awsEndpointURL:            endpoint,
			allowUnsafeAWSEndpointURL: true,
		}

		if err := validateOptions(opts); err == nil {
			t.Fatalf("expected %q to be rejected", endpoint)
		}
	}
}

func TestWithWebhookRuntimeCacheScopeInitializesCacheOptions(t *testing.T) {
	options := cluster.Options{Scheme: testManagerScheme(t)}

	withWebhookRuntimeCacheScope(&options)

	if len(options.Cache.ByObject) == 0 {
		t.Fatal("expected Cache.ByObject to be initialized with webhook runtime entries")
	}

	if options.Client.Cache == nil {
		t.Fatal("expected Client.Cache to be initialized")
	}

	if len(options.Client.Cache.DisableFor) == 0 {
		t.Fatal("expected DisableFor to include webhook runtime live-read entries")
	}
}

func TestWithWebhookRuntimeCacheScopePreservesExistingOptions(t *testing.T) {
	testScheme := testManagerScheme(t)
	configMapSelector := labels.SelectorFromSet(labels.Set{"existing": "configmap"})
	options := cluster.Options{
		Scheme: testScheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {Label: configMapSelector},
			},
		},
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.ConfigMap{}}},
		},
	}

	withWebhookRuntimeCacheScope(&options)

	if options.Scheme != testScheme {
		t.Fatal("expected existing Scheme to be preserved")
	}

	configMapByObject, ok := cacheByObjectForGVK(t, testScheme, options.Cache.ByObject, &corev1.ConfigMap{})
	if !ok {
		t.Fatal("expected existing ConfigMap ByObject entry to be preserved")
	}

	if configMapByObject.Label.String() != configMapSelector.String() {
		t.Fatalf("expected existing ConfigMap selector %q to be preserved, got %q", configMapSelector, configMapByObject.Label)
	}

	if countObjectGVK(t, testScheme, options.Client.Cache.DisableFor, &corev1.ConfigMap{}) != 1 {
		t.Fatalf("expected existing ConfigMap DisableFor entry to be preserved once, got %#v", options.Client.Cache.DisableFor)
	}
}

func TestWithWebhookRuntimeCacheScopePanicsOnConflictingSameGVKByGVK(t *testing.T) {
	testScheme := testManagerScheme(t)
	existingSelector := labels.SelectorFromSet(labels.Set{"environment": "test"})
	options := cluster.Options{
		Scheme: testScheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {Label: existingSelector},
			},
		},
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected conflicting same-GVK ByObject entry to panic")
		}
	}()

	withWebhookRuntimeCacheScope(&options)
}

func TestWithWebhookRuntimeCacheScopeIsIdempotentForSameGVK(t *testing.T) {
	testScheme := testManagerScheme(t)
	options := cluster.Options{Scheme: testScheme}

	if countByObjectGVK(t, testScheme, options.Cache.ByObject, &corev1.Secret{}) != 0 {
		t.Fatalf("expected no preexisting Secret ByObject entry, got %#v", options.Cache.ByObject)
	}

	withWebhookRuntimeCacheScope(&options)

	if countByObjectGVK(t, testScheme, options.Cache.ByObject, &corev1.Secret{}) != 1 {
		t.Fatalf("expected one Secret ByObject entry after first scope option, got %#v", options.Cache.ByObject)
	}

	withWebhookRuntimeCacheScope(&options)

	if countByObjectGVK(t, testScheme, options.Cache.ByObject, &corev1.Secret{}) != 1 {
		t.Fatalf("expected one Secret ByObject entry after second scope option, got %#v", options.Cache.ByObject)
	}
}

func TestWithWebhookRuntimeCacheScopeDeduplicatesDisableForByGVK(t *testing.T) {
	testScheme := testManagerScheme(t)
	options := cluster.Options{
		Scheme: testScheme,
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{
				&corev1.Secret{},
				&corev1.ConfigMap{},
			}},
		},
	}

	withWebhookRuntimeCacheScope(&options)

	if countObjectGVK(t, testScheme, options.Client.Cache.DisableFor, &corev1.Secret{}) != 1 {
		t.Fatalf("expected Secret DisableFor entry to be deduplicated by GVK, got %#v", options.Client.Cache.DisableFor)
	}

	if countObjectGVK(t, testScheme, options.Client.Cache.DisableFor, &corev1.ConfigMap{}) != 1 {
		t.Fatalf("expected ConfigMap DisableFor entry to be preserved, got %#v", options.Client.Cache.DisableFor)
	}

	assertUniqueObjectGVKs(t, testScheme, options.Client.Cache.DisableFor)
}

func TestWithWebhookRuntimeCacheScopeTrimsRemoteServiceAccountCache(t *testing.T) {
	testScheme := testManagerScheme(t)
	options := cluster.Options{Scheme: testScheme}

	withWebhookRuntimeCacheScope(&options)

	byObject, ok := cacheByObjectForGVK(t, testScheme, options.Cache.ByObject, &corev1.ServiceAccount{})
	if !ok {
		t.Fatal("expected ServiceAccount ByObject cache entry")
	}

	if byObject.Transform == nil {
		t.Fatal("expected ServiceAccount cache transform")
	}

	if byObject.Namespaces == nil || len(byObject.Namespaces) != 0 {
		t.Fatalf("expected ServiceAccount cache to explicitly watch all namespaces, got %#v", byObject.Namespaces)
	}

	if countObjectGVK(t, testScheme, options.Client.Cache.DisableFor, &corev1.ServiceAccount{}) != 1 {
		t.Fatalf("expected ServiceAccount client reads to bypass the trimmed cache, got %#v", options.Client.Cache.DisableFor)
	}

	out, err := byObject.Transform(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "app",
			Namespace:     "default",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Secrets:          []corev1.ObjectReference{{Name: "token"}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	sa, ok := out.(*corev1.ServiceAccount)
	if !ok {
		t.Fatalf("expected *corev1.ServiceAccount, got %T", out)
	}

	if len(sa.Secrets) != 0 || len(sa.ImagePullSecrets) != 0 || len(sa.ManagedFields) != 0 {
		t.Fatalf("expected ServiceAccount cache transform to strip unused fields, got %#v", sa)
	}
}

func newExistingServiceAccountLabelTransform(called *bool) toolscache.TransformFunc {
	return func(in any) (any, error) {
		*called = true

		sa, ok := in.(*corev1.ServiceAccount)
		if !ok {
			return in, nil
		}

		if sa.Labels == nil {
			sa.Labels = map[string]string{}
		}

		sa.Labels["existing-transform"] = "true"

		return in, nil
	}
}

func TestWithWebhookRuntimeCacheScopeComposesRemoteServiceAccountTransform(t *testing.T) {
	testScheme := testManagerScheme(t)
	existingTransformCalled := false
	options := cluster.Options{
		Scheme: testScheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ServiceAccount{}: {
					Transform: newExistingServiceAccountLabelTransform(&existingTransformCalled),
				},
			},
		},
	}

	withWebhookRuntimeCacheScope(&options)

	byObject, ok := cacheByObjectForGVK(t, testScheme, options.Cache.ByObject, &corev1.ServiceAccount{})
	if !ok {
		t.Fatal("expected ServiceAccount ByObject cache entry")
	}

	out, err := byObject.Transform(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "app",
			Namespace:     "default",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Secrets:          []corev1.ObjectReference{{Name: "token"}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	sa, ok := out.(*corev1.ServiceAccount)
	if !ok {
		t.Fatalf("expected *corev1.ServiceAccount, got %T", out)
	}

	if !existingTransformCalled {
		t.Fatal("expected existing ServiceAccount transform to run")
	}

	if byObject.Namespaces == nil || len(byObject.Namespaces) != 0 {
		t.Fatalf("expected composed ServiceAccount cache to explicitly watch all namespaces, got %#v", byObject.Namespaces)
	}

	if countObjectGVK(t, testScheme, options.Client.Cache.DisableFor, &corev1.ServiceAccount{}) != 1 {
		t.Fatalf("expected composed ServiceAccount client reads to bypass the trimmed cache, got %#v", options.Client.Cache.DisableFor)
	}

	if sa.Labels["existing-transform"] != "true" {
		t.Fatalf("expected existing ServiceAccount transform label to be preserved, got %#v", sa.Labels)
	}

	if len(sa.Secrets) != 0 || len(sa.ImagePullSecrets) != 0 || len(sa.ManagedFields) != 0 {
		t.Fatalf("expected composed ServiceAccount transform to strip unused fields, got %#v", sa)
	}
}

func TestWithWebhookRuntimeCacheScopePanicsOnRemoteServiceAccountNamespaceRestriction(t *testing.T) {
	testScheme := testManagerScheme(t)
	options := cluster.Options{
		Scheme: testScheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ServiceAccount{}: {
					Namespaces: map[string]cache.Config{
						"default": {},
					},
				},
			},
		},
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected namespace-restricted ServiceAccount ByObject entry to panic")
		}
	}()

	withWebhookRuntimeCacheScope(&options)
}

func TestCacheSyncReadyCheckReturnsErrorWhenCacheNotSynced(t *testing.T) {
	waitForSync := func(_ context.Context) bool {
		return false
	}

	checker := cacheSyncReadyCheck(waitForSync)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)

	err := checker(req)
	if err == nil {
		t.Fatal("expected error when informer cache is not synced so /readyz holds at 503")
	}

	if !strings.Contains(err.Error(), "informer cache not synced") {
		t.Fatalf("expected error message to contain %q, got %q", "informer cache not synced", err.Error())
	}
}

func TestCacheSyncReadyCheckReturnsNilWhenCacheSynced(t *testing.T) {
	waitForSync := func(_ context.Context) bool {
		return true
	}

	checker := cacheSyncReadyCheck(waitForSync)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)

	if err := checker(req); err != nil {
		t.Fatalf("expected nil error when informer cache is synced, got %v", err)
	}
}

type cacheSyncReadyCheckCtxKey struct{}

func TestCacheSyncReadyCheckPropagatesRequestContext(t *testing.T) {
	sentinel := "ready-check-sentinel"

	var observedValue any

	waitForSync := func(ctx context.Context) bool {
		observedValue = ctx.Value(cacheSyncReadyCheckCtxKey{})

		return true
	}

	checker := cacheSyncReadyCheck(waitForSync)
	ctx := context.WithValue(context.Background(), cacheSyncReadyCheckCtxKey{}, sentinel)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", http.NoBody)

	if err := checker(req); err != nil {
		t.Fatalf("expected nil error from synced cache, got %v", err)
	}

	got, ok := observedValue.(string)
	if !ok {
		t.Fatalf("expected request context to be forwarded to waitForSync (sentinel string), got %T %#v", observedValue, observedValue)
	}

	if got != sentinel {
		t.Fatalf("expected sentinel %q to be propagated via the request context, got %q", sentinel, got)
	}
}

func testManagerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	testScheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		admissionregistrationv1.AddToScheme,
		appsv1.AddToScheme,
		corev1.AddToScheme,
		rbacv1.AddToScheme,
	} {
		if err := add(testScheme); err != nil {
			t.Fatal(err)
		}
	}

	return testScheme
}

func cacheByObjectForGVK(t *testing.T, scheme *runtime.Scheme, byObject map[client.Object]cache.ByObject, target client.Object) (cache.ByObject, bool) {
	t.Helper()

	targetGVK := objectGVK(t, scheme, target)
	for obj, config := range byObject {
		if objectGVK(t, scheme, obj) == targetGVK {
			return config, true
		}
	}

	return cache.ByObject{}, false
}

func countByObjectGVK(t *testing.T, scheme *runtime.Scheme, byObject map[client.Object]cache.ByObject, target client.Object) int {
	t.Helper()

	targetGVK := objectGVK(t, scheme, target)
	count := 0

	for obj := range byObject {
		if objectGVK(t, scheme, obj) == targetGVK {
			count++
		}
	}

	return count
}

func countObjectGVK(t *testing.T, scheme *runtime.Scheme, objects []client.Object, target client.Object) int {
	t.Helper()

	targetGVK := objectGVK(t, scheme, target)
	count := 0

	for _, obj := range objects {
		if objectGVK(t, scheme, obj) == targetGVK {
			count++
		}
	}

	return count
}

func assertUniqueObjectGVKs(t *testing.T, scheme *runtime.Scheme, objects []client.Object) {
	t.Helper()

	seen := map[schema.GroupVersionKind]struct{}{}

	for _, obj := range objects {
		gvk := objectGVK(t, scheme, obj)
		if _, ok := seen[gvk]; ok {
			t.Fatalf("expected DisableFor GVKs to be unique, found duplicate %s in %#v", gvk, objects)
		}

		seen[gvk] = struct{}{}
	}
}

func objectGVK(t *testing.T, scheme *runtime.Scheme, obj client.Object) schema.GroupVersionKind {
	t.Helper()

	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		t.Fatal(err)
	}

	return gvk
}
