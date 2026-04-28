package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/record"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

func TestProviderReconcileClusterDoesNotBlockGetWhileCacheSyncWaits(t *testing.T) {
	readyKey := multicluster.ClusterName("ready/ready")
	stuckKey := multicluster.ClusterName("stuck/stuck")
	readyCluster := newFakeRemoteCluster(true)
	stuckCluster := newFakeRemoteCluster(false)
	provider := newTestProvider(Options{
		CacheSyncTimeout: 50 * time.Millisecond,
		NewCluster: func(context.Context, *clusterinventoryv1alpha1.ClusterProfile, *rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			return stuckCluster, nil
		},
	})
	provider.clusters[readyKey] = readyCluster
	provider.kubeconfigs[readyKey] = &rest.Config{Host: "https://ready.example.com"}

	done := make(chan error, 1)

	go func() {
		_, err := provider.reconcileCluster(context.Background(), stuckKey, logr.Discard(), &clusterinventoryv1alpha1.ClusterProfile{}, &rest.Config{Host: "https://stuck.example.com"})
		done <- err
	}()

	waitForSignal(t, stuckCluster.cache.waitStarted, "cache sync to start")

	getDone := make(chan error, 1)

	go func() {
		_, err := provider.Get(context.Background(), readyKey)
		getDone <- err
	}()

	select {
	case err := <-getDone:
		if err != nil {
			t.Fatalf("expected Get to return ready cluster, got %v", err)
		}
	case <-time.After(20 * time.Millisecond):
		t.Fatal("expected Get not to block behind remote cache sync")
	}

	if err := <-done; err == nil {
		t.Fatal("expected stuck cache sync to time out")
	}
}

func TestProviderReconcileClusterCacheSyncTimeoutDoesNotPublishCluster(t *testing.T) {
	key := multicluster.ClusterName("stuck/stuck")
	stuckCluster := newFakeRemoteCluster(false)
	engager := &fakeClusterEngager{}
	provider := newTestProvider(Options{
		CacheSyncTimeout: 10 * time.Millisecond,
		NewCluster: func(context.Context, *clusterinventoryv1alpha1.ClusterProfile, *rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			return stuckCluster, nil
		},
	})
	provider.mcMgr = engager

	if _, err := provider.reconcileCluster(context.Background(), key, logr.Discard(), &clusterinventoryv1alpha1.ClusterProfile{}, &rest.Config{Host: "https://stuck.example.com"}); err == nil {
		t.Fatal("expected cache sync timeout error")
	}

	if _, err := provider.Get(context.Background(), key); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Fatalf("expected timed-out cluster not to be published, got %v", err)
	}

	if engager.engaged != 0 {
		t.Fatalf("expected timed-out cluster not to be engaged, got %d engagements", engager.engaged)
	}
}

func TestProviderReconcileClusterStartErrorDoesNotPublishCluster(t *testing.T) {
	key := multicluster.ClusterName("broken/broken")
	startErr := errors.New("start failed")
	brokenCluster := newFakeRemoteCluster(false)
	brokenCluster.startErr = startErr
	engager := &fakeClusterEngager{}
	provider := newTestProvider(Options{
		CacheSyncTimeout: time.Second,
		NewCluster: func(context.Context, *clusterinventoryv1alpha1.ClusterProfile, *rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			return brokenCluster, nil
		},
	})
	provider.mcMgr = engager

	if _, err := provider.reconcileCluster(context.Background(), key, logr.Discard(), &clusterinventoryv1alpha1.ClusterProfile{}, &rest.Config{Host: "https://broken.example.com"}); !errors.Is(err, startErr) {
		t.Fatalf("expected start error, got %v", err)
	}

	if _, err := provider.Get(context.Background(), key); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Fatalf("expected failed cluster not to be published, got %v", err)
	}

	if engager.engaged != 0 {
		t.Fatalf("expected failed cluster not to be engaged, got %d engagements", engager.engaged)
	}
}

func TestProviderReconcileClusterSteadyRequeuesAfterSuccessfulEngage(t *testing.T) {
	key := multicluster.ClusterName("ready/ready")
	stopCluster := make(chan struct{})
	readyCluster := newFakeRemoteCluster(true)
	readyCluster.stop = stopCluster
	engager := &fakeClusterEngager{}
	provider := newTestProvider(Options{
		NewCluster: func(context.Context, *clusterinventoryv1alpha1.ClusterProfile, *rest.Config, ...cluster.Option) (cluster.Cluster, error) {
			return readyCluster, nil
		},
	})
	provider.mcMgr = engager

	result, err := provider.reconcileCluster(context.Background(), key, logr.Discard(), &clusterinventoryv1alpha1.ClusterProfile{}, &rest.Config{Host: "https://ready.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if result.RequeueAfter != providerSteadyStateRequeue {
		t.Fatalf("expected steady-state requeue %s, got %#v", providerSteadyStateRequeue, result)
	}

	if _, err := provider.Get(context.Background(), key); err != nil {
		t.Fatalf("expected engaged cluster to be published, got %v", err)
	}

	if engager.engaged != 1 {
		t.Fatalf("expected cluster to be engaged once, got %d", engager.engaged)
	}

	close(stopCluster)
	waitForClusterGone(t, provider, key)
}

func TestProviderReconcileDisengagesNotReadyClusterProfile(t *testing.T) {
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "target", Name: "target"}}
	key := multicluster.ClusterName(request.String())
	profile := newTestClusterProfile("http://proxy.example.com:8080")
	profile.ObjectMeta = metav1.ObjectMeta{Namespace: "target", Name: "target"}
	provider, cancelCtx := newTestProviderWithConfig(key, &rest.Config{Host: "https://stale.example.com"})
	provider.client = newTestInventoryClient(t, profile)
	provider.accessConfig = newTestAccessConfig("--cluster", "target")
	provider.opts.IsReady = func(context.Context, *clusterinventoryv1alpha1.ClusterProfile) bool {
		return false
	}

	result, err := provider.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	if result.RequeueAfter != providerSteadyStateRequeue {
		t.Fatalf("expected not-ready requeue %s, got %#v", providerSteadyStateRequeue, result)
	}

	if _, err := provider.Get(context.Background(), key); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Fatalf("expected not-ready ClusterProfile to disengage stale cluster, got %v", err)
	}

	assertClusterDisengaged(t, provider, key, cancelCtx)
}

func TestProviderReconcileDisengagesClusterProfileWhenConfigBuildFails(t *testing.T) {
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "target", Name: "target"}}
	key := multicluster.ClusterName(request.String())
	profile := newTestClusterProfile("http://proxy.example.com:8080")
	profile.ObjectMeta = metav1.ObjectMeta{Namespace: "target", Name: "target"}
	provider, cancelCtx := newTestProviderWithConfig(key, &rest.Config{Host: "https://stale.example.com"})
	provider.client = newTestInventoryClient(t, profile)
	provider.opts.IsReady = func(context.Context, *clusterinventoryv1alpha1.ClusterProfile) bool {
		return true
	}

	if _, err := provider.Reconcile(context.Background(), request); err == nil {
		t.Fatal("expected config build failure")
	}

	if _, err := provider.Get(context.Background(), key); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Fatalf("expected config build failure to disengage stale cluster, got %v", err)
	}

	assertClusterDisengaged(t, provider, key, cancelCtx)
}

// Two ClusterProfiles can resolve to the same logical remote cluster when the
// `open-cluster-management.io/cluster-name` label collides (OCM access
// providers may create one ClusterProfile per ManagedClusterSet binding for
// the same underlying ManagedCluster). Deleting or marking one of them
// not-ready must not cancel the shared cluster context while the other
// ClusterProfile still references it.
func TestProviderDisengageProfileKeepsSharedLogicalClusterReferencedByAnotherProfile(t *testing.T) {
	logicalKey := multicluster.ClusterName("target/target")
	first := multicluster.ClusterName("set-a/target")
	second := multicluster.ClusterName("set-b/target")

	provider, cancelCtx := newTestProviderWithConfig(logicalKey, &rest.Config{Host: "https://target.example.com"})
	provider.profileKeys[first] = logicalKey
	provider.profileKeys[second] = logicalKey

	provider.disengageProfile(first, first)

	if _, ok := provider.clusters[logicalKey]; !ok {
		t.Fatal("expected shared logical cluster to remain engaged while another ClusterProfile references it")
	}

	select {
	case <-cancelCtx.Done():
		t.Fatal("expected shared cluster context to remain live while another ClusterProfile references it")
	default:
	}

	if _, ok := provider.profileKeys[first]; ok {
		t.Fatalf("expected profileKeys[%s] to be removed after disengage", first)
	}

	if mapped, ok := provider.profileKeys[second]; !ok || mapped != logicalKey {
		t.Fatalf("expected profileKeys[%s] to still map to %s, got %v (%v)", second, logicalKey, mapped, ok)
	}

	provider.disengageProfile(second, second)

	assertClusterDisengaged(t, provider, logicalKey, cancelCtx)
}

// rememberProfileKey reassigns a ClusterProfile from one logical key to
// another (e.g. the OCM cluster-name label flips). The old logical cluster
// context must survive when another surviving ClusterProfile still maps to it.
func TestProviderRememberProfileKeyKeepsOldLogicalClusterReferencedByAnotherProfile(t *testing.T) {
	oldLogical := multicluster.ClusterName("old/old")
	newLogical := multicluster.ClusterName("new/new")
	moving := multicluster.ClusterName("set-a/cp")
	pinned := multicluster.ClusterName("set-b/cp")

	provider, cancelCtx := newTestProviderWithConfig(oldLogical, &rest.Config{Host: "https://old.example.com"})
	provider.profileKeys[moving] = oldLogical
	provider.profileKeys[pinned] = oldLogical

	provider.rememberProfileKey(moving, newLogical)

	if _, ok := provider.clusters[oldLogical]; !ok {
		t.Fatal("expected old logical cluster to remain engaged while a second ClusterProfile references it")
	}

	select {
	case <-cancelCtx.Done():
		t.Fatal("expected old logical cluster context to remain live while another ClusterProfile references it")
	default:
	}

	if mapped, ok := provider.profileKeys[moving]; !ok || mapped != newLogical {
		t.Fatalf("expected profileKeys[%s] to map to %s, got %v (%v)", moving, newLogical, mapped, ok)
	}

	if mapped, ok := provider.profileKeys[pinned]; !ok || mapped != oldLogical {
		t.Fatalf("expected profileKeys[%s] to still map to %s, got %v (%v)", pinned, oldLogical, mapped, ok)
	}
}

func TestProviderIndexFieldDoesNotBlockGet(t *testing.T) {
	key := multicluster.ClusterName("ready/ready")
	readyCluster := newFakeRemoteCluster(true)
	blockIndex := make(chan struct{})
	readyCluster.cache.indexBlock = blockIndex
	provider := newTestProvider(Options{})
	provider.clusters[key] = readyCluster
	provider.kubeconfigs[key] = &rest.Config{Host: "https://ready.example.com"}

	indexDone := make(chan error, 1)

	go func() {
		indexDone <- provider.IndexField(context.Background(), &corev1.ConfigMap{}, "metadata.name", func(obj client.Object) []string {
			return []string{obj.GetName()}
		})
	}()

	waitForSignal(t, readyCluster.cache.indexStarted, "index registration to start")

	getDone := make(chan error, 1)

	go func() {
		_, err := provider.Get(context.Background(), key)
		getDone <- err
	}()

	select {
	case err := <-getDone:
		if err != nil {
			t.Fatalf("expected Get to return while IndexField is blocked, got %v", err)
		}
	case <-time.After(20 * time.Millisecond):
		t.Fatal("expected Get not to block behind IndexField")
	}

	close(blockIndex)

	if err := <-indexDone; err != nil {
		t.Fatal(err)
	}
}

func TestProviderKeepExistingClusterKeepsEquivalentBuildConfigFromCPConfigs(t *testing.T) {
	key := multicluster.ClusterName("target/target")
	accessConfig := newTestAccessConfig("--cluster", "target")
	profile := newTestClusterProfile("http://proxy.example.com:8080")
	existingCfg := buildTestConfigFromCP(t, accessConfig, profile)
	nextCfg := buildTestConfigFromCP(t, accessConfig, profile)
	provider := newTestProvider(Options{})
	provider.clusters[key] = newFakeRemoteCluster(true)
	provider.kubeconfigs[key] = existingCfg

	if !provider.keepExistingCluster(key, nextCfg) {
		t.Fatal("expected equivalent BuildConfigFromCP configs to keep the existing cluster")
	}
}

func TestProviderKeepExistingClusterDetectsBuildConfigFromCPAccessProviderConfigChange(t *testing.T) {
	key := multicluster.ClusterName("target/target")
	profile := newTestClusterProfile("http://proxy.example.com:8080")
	existingCfg := buildTestConfigFromCP(t, newTestAccessConfig("--cluster", "target"), profile)
	nextCfg := buildTestConfigFromCP(t, newTestAccessConfig("--cluster", "changed"), profile)
	provider, cancelCtx := newTestProviderWithConfig(key, existingCfg)

	if provider.keepExistingCluster(key, nextCfg) {
		t.Fatal("expected access provider config changes to disengage the existing cluster")
	}

	assertClusterDisengaged(t, provider, key, cancelCtx)
}

func TestProviderKeepExistingClusterDetectsBuildConfigFromCPProxyChange(t *testing.T) {
	key := multicluster.ClusterName("target/target")
	accessConfig := newTestAccessConfig("--cluster", "target")
	existingCfg := buildTestConfigFromCP(t, accessConfig, newTestClusterProfile("http://proxy-a.example.com:8080"))
	nextCfg := buildTestConfigFromCP(t, accessConfig, newTestClusterProfile("http://proxy-b.example.com:8080"))
	provider, cancelCtx := newTestProviderWithConfig(key, existingCfg)

	if provider.keepExistingCluster(key, nextCfg) {
		t.Fatal("expected ClusterProfile proxy changes to disengage the existing cluster")
	}

	assertClusterDisengaged(t, provider, key, cancelCtx)
}

func TestBuildConfigFromClusterProfileDoesNotMutateAccessConfigAdditionalArgs(t *testing.T) {
	accessConfig := newTestAccessConfigWithAdditionalArgsPolicy("--cluster", "target")
	profile := newTestClusterProfile("http://proxy.example.com:8080", clientcmdv1.NamedExtension{
		Name:      "clusterprofiles.multicluster.x-k8s.io/exec/additional-args",
		Extension: runtime.RawExtension{Raw: []byte("- --audience\n- target\n")},
	})

	first := buildTestConfigFromCP(t, accessConfig, profile)
	second := buildTestConfigFromCP(t, accessConfig, profile)
	wantArgs := []string{"--cluster", "target", "--audience", "target"}

	if !slices.Equal(first.ExecProvider.Args, wantArgs) {
		t.Fatalf("expected first exec args %#v, got %#v", wantArgs, first.ExecProvider.Args)
	}

	if !slices.Equal(second.ExecProvider.Args, wantArgs) {
		t.Fatalf("expected second exec args not to accumulate, want %#v, got %#v", wantArgs, second.ExecProvider.Args)
	}
}

func TestRestConfigEqualComparesConnectionAndAuthFields(t *testing.T) {
	if !restConfigEqual(newComparableRestConfig(), newComparableRestConfig()) {
		t.Fatal("expected equivalent rest.Config values to compare equal")
	}

	for _, test := range restConfigInequalityCases() {
		t.Run(test.name, func(t *testing.T) {
			existing := newComparableRestConfig()
			next := newComparableRestConfig()
			test.mutate(next)

			if restConfigEqual(existing, next) {
				t.Fatalf("expected %s difference to compare unequal", test.name)
			}
		})
	}
}

func restConfigInequalityCases() []struct {
	name   string
	mutate func(*rest.Config)
} {
	return []struct {
		name   string
		mutate func(*rest.Config)
	}{
		{name: "exec api version", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.APIVersion = "client.authentication.k8s.io/v1beta1"
		}},
		{name: "exec command", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.Command = "other-auth"
		}},
		{name: "exec args", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.Args = []string{"--cluster", "other"}
		}},
		{name: "exec env", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.Env = []clientcmdapi.ExecEnvVar{{Name: "AWS_PROFILE", Value: "other"}}
		}},
		{name: "exec provide cluster info", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.ProvideClusterInfo = false
		}},
		{name: "exec interactive mode", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.InteractiveMode = clientcmdapi.AlwaysExecInteractiveMode
		}},
		{name: "exec install hint", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.InstallHint = "install other-auth"
		}},
		{name: "exec config", mutate: func(cfg *rest.Config) {
			cfg.ExecProvider.Config = &runtime.Unknown{Raw: []byte(`{"audience":"other"}`)}
		}},
		{name: "proxy", mutate: func(cfg *rest.Config) {
			cfg.Proxy = testProxy("http://proxy-b.example.com:8080")
		}},
		{name: "impersonate groups", mutate: func(cfg *rest.Config) {
			cfg.Impersonate.Groups = []string{"system:masters"}
		}},
		{name: "impersonate extra", mutate: func(cfg *rest.Config) {
			cfg.Impersonate.Extra = map[string][]string{"scope": {"write"}}
		}},
		{name: "impersonate uid", mutate: func(cfg *rest.Config) {
			cfg.Impersonate.UID = "other-uid"
		}},
		{name: "auth provider config", mutate: func(cfg *rest.Config) {
			cfg.AuthProvider.Config["id-token"] = "other-token"
		}},
		{name: "tls next protos", mutate: func(cfg *rest.Config) {
			cfg.NextProtos = []string{"http/1.1"}
		}},
		{name: "transport hook presence", mutate: func(cfg *rest.Config) {
			cfg.WrapTransport = nil
		}},
	}
}

func TestRestConfigEqualTreatsExecEnvAsEffectiveEnvironment(t *testing.T) {
	existing := newComparableRestConfig()
	next := newComparableRestConfig()
	existing.ExecProvider.Env = []clientcmdapi.ExecEnvVar{
		{Name: "AWS_REGION", Value: "us-east-1"},
		{Name: "AWS_PROFILE", Value: "target"},
	}
	next.ExecProvider.Env = []clientcmdapi.ExecEnvVar{
		{Name: "AWS_PROFILE", Value: "target"},
		{Name: "AWS_REGION", Value: "us-east-1"},
	}

	if !restConfigEqual(existing, next) {
		t.Fatal("expected reordered exec env with the same effective values to compare equal")
	}
}

func TestRestConfigEqualDoesNotCompareFunctionPointerIdentity(t *testing.T) {
	existing := &rest.Config{
		Host:  "https://cluster.example.com",
		Proxy: testProxy("http://proxy.example.com:8080"),
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			return rt
		},
	}
	next := &rest.Config{
		Host:  "https://cluster.example.com",
		Proxy: testProxy("http://proxy.example.com:8080"),
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			return rt
		},
	}

	if !restConfigEqual(existing, next) {
		t.Fatal("expected equivalent function-backed configs to compare equal")
	}
}

func newTestProvider(opts Options) *Provider {
	setProviderDefaults(&opts)

	return &Provider{
		opts:        opts,
		log:         logr.Discard(),
		mcMgr:       &fakeClusterEngager{},
		clusters:    map[multicluster.ClusterName]cluster.Cluster{},
		cancelFns:   map[multicluster.ClusterName]context.CancelFunc{},
		kubeconfigs: map[multicluster.ClusterName]*rest.Config{},
		profileKeys: map[multicluster.ClusterName]multicluster.ClusterName{},
	}
}

func newTestInventoryClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		Build()
}

const testAccessProviderName = "test-access"

func newTestAccessConfig(args ...string) *access.Config {
	return access.New([]access.Provider{{
		Name: testAccessProviderName,
		ExecConfig: &clientcmdapi.ExecConfig{
			APIVersion:         "client.authentication.k8s.io/v1",
			Command:            "test-auth",
			Args:               args,
			Env:                []clientcmdapi.ExecEnvVar{{Name: "AWS_PROFILE", Value: "target"}},
			InstallHint:        "install test-auth",
			ProvideClusterInfo: true,
			InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
		},
	}})
}

func newTestAccessConfigWithAdditionalArgsPolicy(args ...string) *access.Config {
	config := newTestAccessConfig(args...)
	config.Providers[0].ProfileSourcedCLIArgsPolicy = access.ProfileSourcedCLIArgsPolicyAppend

	return config
}

func newTestClusterProfile(proxyURL string, extraExtensions ...clientcmdv1.NamedExtension) *clusterinventoryv1alpha1.ClusterProfile {
	extensions := make([]clientcmdv1.NamedExtension, 0, 1+len(extraExtensions))
	extensions = append(extensions, clientcmdv1.NamedExtension{
		Name:      "client.authentication.k8s.io/exec",
		Extension: runtime.RawExtension{Raw: []byte(`{"audience":"target"}`)},
	})
	extensions = append(extensions, extraExtensions...)

	return &clusterinventoryv1alpha1.ClusterProfile{
		Status: clusterinventoryv1alpha1.ClusterProfileStatus{
			AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{
				Name: testAccessProviderName,
				Cluster: clientcmdv1.Cluster{
					Server:                   "https://cluster.example.com",
					CertificateAuthorityData: []byte("test-ca"),
					ProxyURL:                 proxyURL,
					Extensions:               extensions,
				},
			}},
		},
	}
}

func buildTestConfigFromCP(t *testing.T, accessConfig *access.Config, profile *clusterinventoryv1alpha1.ClusterProfile) *rest.Config {
	t.Helper()

	cfg, err := buildConfigFromClusterProfile(accessConfig, profile)
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func newTestProviderWithConfig(key multicluster.ClusterName, cfg *rest.Config) (*Provider, context.Context) {
	provider := newTestProvider(Options{})
	//nolint:gosec // cancel is stored on provider.cancelFns and invoked via disengage paths under test
	ctx, cancel := context.WithCancel(context.Background())

	provider.clusters[key] = newFakeRemoteCluster(true)
	provider.cancelFns[key] = cancel
	provider.kubeconfigs[key] = cfg

	return provider, ctx
}

// cancelCtx is intentionally last: *testing.T leads test helpers by convention.
//
//nolint:revive // context-as-argument
func assertClusterDisengaged(t *testing.T, provider *Provider, key multicluster.ClusterName, cancelCtx context.Context) {
	t.Helper()

	if _, ok := provider.clusters[key]; ok {
		t.Fatalf("expected cluster %s to be removed", key)
	}

	if _, ok := provider.kubeconfigs[key]; ok {
		t.Fatalf("expected kubeconfig for cluster %s to be removed", key)
	}

	select {
	case <-cancelCtx.Done():
	default:
		t.Fatalf("expected cancel func for cluster %s to be called", key)
	}
}

// newComparableRestConfig synthesises a rest.Config with every comparable
// credential field populated for restConfigEqual coverage.
//
//nolint:gosec // test fixture; G101 false positive on rest.Config credential fields
func newComparableRestConfig() *rest.Config {
	return &rest.Config{
		Host:            "https://cluster.example.com",
		APIPath:         "/api",
		Username:        "user",
		Password:        "password",
		BearerToken:     "token",
		BearerTokenFile: "/var/run/token",
		Impersonate: rest.ImpersonationConfig{
			UserName: "impersonated-user",
			UID:      "impersonated-uid",
			Groups:   []string{"developers", "auditors"},
			Extra:    map[string][]string{"scope": {"read", "write"}},
		},
		AuthProvider: &clientcmdapi.AuthProviderConfig{
			Name:   "oidc",
			Config: map[string]string{"id-token": "token"},
		},
		ExecProvider: &clientcmdapi.ExecConfig{
			APIVersion:         "client.authentication.k8s.io/v1",
			Command:            "test-auth",
			Args:               []string{"--cluster", "target"},
			Env:                []clientcmdapi.ExecEnvVar{{Name: "AWS_PROFILE", Value: "target"}},
			InstallHint:        "install test-auth",
			ProvideClusterInfo: true,
			Config:             &runtime.Unknown{Raw: []byte(`{"audience":"target"}`)},
			InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
			PluginPolicy: clientcmdapi.PluginPolicy{
				PolicyType: clientcmdapi.PluginPolicyAllowlist,
				Allowlist:  []clientcmdapi.AllowlistEntry{{Name: "test-auth"}},
			},
		},
		TLSClientConfig: rest.TLSClientConfig{
			Insecure:   false,
			ServerName: "cluster.example.com",
			CertFile:   "/var/run/cert",
			KeyFile:    "/var/run/key",
			CAFile:     "/var/run/ca",
			CertData:   []byte("cert"),
			KeyData:    []byte("key"),
			CAData:     []byte("ca"),
			NextProtos: []string{"h2"},
		},
		Proxy: testProxy("http://proxy-a.example.com:8080"),
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			return rt
		},
	}
}

func testProxy(rawURL string) func(*http.Request) (*url.URL, error) {
	return func(*http.Request) (*url.URL, error) {
		return url.Parse(rawURL)
	}
}

func waitForSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForClusterGone(t *testing.T, provider *Provider, key multicluster.ClusterName) {
	t.Helper()

	deadline := time.After(time.Second)

	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for {
		_, err := provider.Get(context.Background(), key)
		if errors.Is(err, multicluster.ErrClusterNotFound) {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for cluster %s to be disengaged", key)
		case <-tick.C:
		}
	}
}

type fakeClusterEngager struct {
	engaged int
}

func (e *fakeClusterEngager) Engage(context.Context, multicluster.ClusterName, cluster.Cluster) error {
	e.engaged++

	return nil
}

type fakeRemoteCluster struct {
	cache        *fakeRemoteCache
	scheme       *runtime.Scheme
	startErr     error
	startStarted chan struct{}
	startOnce    sync.Once
	stop         <-chan struct{}
}

func newFakeRemoteCluster(cacheSynced bool) *fakeRemoteCluster {
	return &fakeRemoteCluster{
		cache:        newFakeRemoteCache(cacheSynced),
		scheme:       runtime.NewScheme(),
		startStarted: make(chan struct{}),
	}
}

func (c *fakeRemoteCluster) GetHTTPClient() *http.Client { return http.DefaultClient }
func (c *fakeRemoteCluster) GetConfig() *rest.Config     { return &rest.Config{} }
func (c *fakeRemoteCluster) GetCache() cache.Cache       { return c.cache }
func (c *fakeRemoteCluster) GetScheme() *runtime.Scheme  { return c.scheme }
func (c *fakeRemoteCluster) GetClient() client.Client    { return nil }
func (c *fakeRemoteCluster) GetFieldIndexer() client.FieldIndexer {
	return c.cache
}
func (c *fakeRemoteCluster) GetRESTMapper() meta.RESTMapper { return nil }
func (c *fakeRemoteCluster) GetAPIReader() client.Reader    { return nil }
func (c *fakeRemoteCluster) GetEventRecorderFor(string) record.EventRecorder {
	return nil
}
func (c *fakeRemoteCluster) GetEventRecorder(string) events.EventRecorder {
	return nil
}
func (c *fakeRemoteCluster) Start(ctx context.Context) error {
	c.startOnce.Do(func() {
		close(c.startStarted)
	})

	if c.startErr != nil {
		return c.startErr
	}

	if c.stop == nil {
		<-ctx.Done()

		return fmt.Errorf("fake remote cluster Start ctx done: %w", ctx.Err())
	}

	select {
	case <-c.stop:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("fake remote cluster Start ctx done: %w", ctx.Err())
	}
}

var errFakeInformerUnused = errors.New("fakeRemoteCache informer accessor not exercised by tests")

type fakeRemoteCache struct {
	cacheSynced  bool
	waitStarted  chan struct{}
	waitOnce     sync.Once
	indexStarted chan struct{}
	indexOnce    sync.Once
	indexBlock   <-chan struct{}
}

func newFakeRemoteCache(cacheSynced bool) *fakeRemoteCache {
	return &fakeRemoteCache{
		cacheSynced:  cacheSynced,
		waitStarted:  make(chan struct{}),
		indexStarted: make(chan struct{}),
	}
}

func (c *fakeRemoteCache) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return nil
}
func (c *fakeRemoteCache) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}
func (c *fakeRemoteCache) GetInformer(context.Context, client.Object, ...cache.InformerGetOption) (cache.Informer, error) {
	return nil, errFakeInformerUnused
}
func (c *fakeRemoteCache) GetInformerForKind(context.Context, schema.GroupVersionKind, ...cache.InformerGetOption) (cache.Informer, error) {
	return nil, errFakeInformerUnused
}
func (c *fakeRemoteCache) RemoveInformer(context.Context, client.Object) error { return nil }
func (c *fakeRemoteCache) Start(ctx context.Context) error {
	<-ctx.Done()

	return fmt.Errorf("fake remote cache Start ctx done: %w", ctx.Err())
}
func (c *fakeRemoteCache) WaitForCacheSync(ctx context.Context) bool {
	c.waitOnce.Do(func() {
		close(c.waitStarted)
	})

	if c.cacheSynced {
		return true
	}

	<-ctx.Done()

	return false
}
func (c *fakeRemoteCache) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	c.indexOnce.Do(func() {
		close(c.indexStarted)
	})

	if c.indexBlock != nil {
		<-c.indexBlock
	}

	return nil
}
