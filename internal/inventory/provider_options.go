// Package inventory resolves ClusterProfile resources and manages remote clusters.
package inventory

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

var _ multicluster.Provider = &Provider{}

// Options customizes ClusterProfile-backed cluster creation.
type Options struct {
	ClusterOptions   []cluster.Option
	NewCluster       func(ctx context.Context, profile *clusterinventoryv1alpha1.ClusterProfile, cfg *rest.Config, opts ...cluster.Option) (cluster.Cluster, error)
	IsReady          func(ctx context.Context, profile *clusterinventoryv1alpha1.ClusterProfile) bool
	CacheSyncTimeout time.Duration
}

// Provider adapts ClusterProfile inventory into multicluster-runtime clusters.
type Provider struct {
	opts         Options
	accessConfig *access.Config
	log          logr.Logger
	client       client.Client
	lock         sync.RWMutex
	opsLock      sync.Mutex
	mcMgr        clusterEngager
	clusters     map[multicluster.ClusterName]cluster.Cluster
	cancelFns    map[multicluster.ClusterName]context.CancelFunc
	kubeconfigs  map[multicluster.ClusterName]*rest.Config
	profileKeys  map[multicluster.ClusterName]multicluster.ClusterName
	indexers     []index
}

type index struct {
	object       client.Object
	field        string
	extractValue client.IndexerFunc
}

type clusterEngager interface {
	Engage(context.Context, multicluster.ClusterName, cluster.Cluster) error
}

const (
	defaultCacheSyncTimeout    = 30 * time.Second
	providerSteadyStateRequeue = time.Minute
)

// NewProviderFromFile creates a Provider from a ClusterProfile access config file.
func NewProviderFromFile(path string, opts ...Options) (*Provider, error) {
	accessConfig, err := access.NewFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ClusterProfile access provider file: %w", err)
	}

	var option Options
	if len(opts) > 0 {
		option = opts[0]
	}

	setProviderDefaults(&option)

	return &Provider{
		opts:         option,
		accessConfig: accessConfig,
		log:          log.Log.WithName("cluster-inventory-api-cluster-provider"),
		clusters:     map[multicluster.ClusterName]cluster.Cluster{},
		cancelFns:    map[multicluster.ClusterName]context.CancelFunc{},
		kubeconfigs:  map[multicluster.ClusterName]*rest.Config{},
		profileKeys:  map[multicluster.ClusterName]multicluster.ClusterName{},
	}, nil
}

func setProviderDefaults(opts *Options) {
	if opts.NewCluster == nil {
		opts.NewCluster = func(_ context.Context, _ *clusterinventoryv1alpha1.ClusterProfile, cfg *rest.Config, opts ...cluster.Option) (cluster.Cluster, error) {
			return cluster.New(cfg, opts...)
		}
	}

	if opts.IsReady == nil {
		opts.IsReady = func(_ context.Context, profile *clusterinventoryv1alpha1.ClusterProfile) bool {
			condition := meta.FindStatusCondition(profile.Status.Conditions, clusterinventoryv1alpha1.ClusterConditionControlPlaneHealthy)

			return condition != nil && condition.Status == metav1.ConditionTrue
		}
	}

	if opts.CacheSyncTimeout <= 0 {
		opts.CacheSyncTimeout = defaultCacheSyncTimeout
	}
}

// SetupWithManager registers the provider's local ClusterProfile controller.
func (p *Provider) SetupWithManager(mgr mcmanager.Manager) error {
	if mgr == nil {
		return fmt.Errorf("manager is nil")
	}

	p.mcMgr = mgr

	localMgr := mgr.GetLocalManager()
	if localMgr == nil {
		return fmt.Errorf("local manager is nil")
	}

	p.client = localMgr.GetClient()

	if err := builder.ControllerManagedBy(localMgr).
		For(&clusterinventoryv1alpha1.ClusterProfile{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(p); err != nil {
		return fmt.Errorf("set up ClusterProfile provider controller: %w", err)
	}

	return nil
}

// Get returns an engaged remote cluster by name.
func (p *Provider) Get(_ context.Context, clusterName multicluster.ClusterName) (cluster.Cluster, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if cl, ok := p.clusters[clusterName]; ok {
		return cl, nil
	}

	return nil, multicluster.ErrClusterNotFound
}

// Reconcile engages or refreshes a remote cluster for one ClusterProfile.
func (p *Provider) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	requestKey := multicluster.ClusterName(req.String())
	logger := p.log.WithValues("clusterprofile", requestKey)

	profile := &clusterinventoryv1alpha1.ClusterProfile{}
	if err := p.client.Get(ctx, req.NamespacedName, profile); err != nil {
		if apierrors.IsNotFound(err) {
			p.disengageProfile(requestKey, multicluster.ClusterName((types.NamespacedName{Namespace: req.Name, Name: req.Name}).String()))

			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("get ClusterProfile %s: %w", requestKey, err)
	}

	key := multicluster.ClusterName(logicalClusterName(profile).String())
	logger = logger.WithValues("remoteCluster", key)

	if p.mcMgr == nil {
		return reconcile.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if !p.opts.IsReady(ctx, profile) {
		p.disengageProfile(requestKey, key)

		return reconcile.Result{RequeueAfter: providerSteadyStateRequeue}, nil
	}

	cfg, err := buildConfigFromClusterProfile(p.accessConfig, profile)
	if err != nil {
		p.disengageProfile(requestKey, key)

		return reconcile.Result{}, fmt.Errorf("build kubeconfig for ClusterProfile %s: %w", requestKey, err)
	}

	p.rememberProfileKey(requestKey, key)

	return p.reconcileCluster(ctx, key, logger, profile, cfg)
}

func (p *Provider) reconcileCluster(ctx context.Context, key multicluster.ClusterName, logger logr.Logger, profile *clusterinventoryv1alpha1.ClusterProfile, cfg *rest.Config) (reconcile.Result, error) {
	p.opsLock.Lock()
	defer p.opsLock.Unlock()

	if p.keepExistingCluster(key, cfg) {
		return reconcile.Result{RequeueAfter: providerSteadyStateRequeue}, nil
	}

	indexers := p.snapshotIndexers()

	cl, err := p.opts.NewCluster(ctx, profile, cfg, p.opts.ClusterOptions...)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("create cluster for ClusterProfile %s: %w", key, err)
	}

	for _, idx := range indexers {
		if err := cl.GetCache().IndexField(ctx, idx.object, idx.field, idx.extractValue); err != nil {
			return reconcile.Result{}, fmt.Errorf("index field %q for ClusterProfile %s: %w", idx.field, key, err)
		}
	}

	clusterCtx, cancel := context.WithCancel(ctx)
	cleanupCluster := true

	defer func() {
		if cleanupCluster {
			cancel()
		}
	}()

	startErr := make(chan error, 1)
	runState := &clusterRunState{}

	go p.monitorClusterStart(clusterCtx, key, cl, logger, runState, startErr)

	synced, err := waitForRemoteCacheSync(clusterCtx, cl, p.cacheSyncTimeout(), startErr)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("start ClusterProfile %s: %w", key, err)
	}

	if !synced {
		return reconcile.Result{}, fmt.Errorf("sync cache for ClusterProfile %s", key)
	}

	if err := p.publishAndEngage(clusterCtx, key, cl, cfg, cancel, runState); err != nil {
		return reconcile.Result{}, err
	}

	cleanupCluster = false

	return reconcile.Result{RequeueAfter: providerSteadyStateRequeue}, nil
}

// publishAndEngage publishes the cluster, engages it with the multicluster
// manager, and verifies the goroutine has not since stopped at each step.
// runState is rechecked between publish, engage, and current-cluster guards
// because the start goroutine can mark the cluster stopped concurrently.
func (p *Provider) publishAndEngage(clusterCtx context.Context, key multicluster.ClusterName, cl cluster.Cluster, cfg *rest.Config, cancel context.CancelFunc, runState *clusterRunState) error {
	if err := runState.errIfStopped(); err != nil {
		return fmt.Errorf("start ClusterProfile %s: %w", key, err)
	}

	p.publishCluster(key, cl, cancel, cfg)

	if err := runState.errIfStopped(); err != nil {
		p.disengageIfCurrent(key, cl)

		return fmt.Errorf("start ClusterProfile %s: %w", key, err)
	}

	if err := p.mcMgr.Engage(clusterCtx, key, cl); err != nil {
		p.disengageIfCurrent(key, cl)

		return fmt.Errorf("engage ClusterProfile %s: %w", key, err)
	}

	if !p.isCurrentCluster(key, cl) {
		return fmt.Errorf("remote cluster for ClusterProfile %s stopped while engaging", key)
	}

	if err := runState.errIfStopped(); err != nil {
		p.disengageIfCurrent(key, cl)

		return fmt.Errorf("start ClusterProfile %s: %w", key, err)
	}

	return nil
}

// monitorClusterStart drives a remote cluster's Start lifecycle. It records the
// terminating error (if any) on runState and disengages the cluster on
// unexpected termination. The non-blocking send into startErr only matters for
// the first listener (waitForRemoteCacheSync); later sends are dropped.
func (p *Provider) monitorClusterStart(clusterCtx context.Context, key multicluster.ClusterName, cl cluster.Cluster, logger logr.Logger, runState *clusterRunState, startErr chan<- error) {
	err := cl.Start(clusterCtx)
	if clusterCtx.Err() != nil {
		return
	}

	if err == nil {
		err = fmt.Errorf("remote cluster stopped unexpectedly")
		logger.Error(err, "remote cluster stopped before context cancellation")
	} else {
		logger.Error(err, "failed to start remote cluster")
	}

	runState.markStopped(err)
	p.disengageIfCurrent(key, cl)

	select {
	case startErr <- err:
	default:
	}
}

func (p *Provider) keepExistingCluster(key multicluster.ClusterName, cfg *rest.Config) bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	if _, ok := p.clusters[key]; !ok {
		return false
	}

	if p.kubeconfigs[key] != nil && restConfigEqual(p.kubeconfigs[key], cfg) {
		return true
	}

	p.disengageLocked(key)

	return false
}

func (p *Provider) snapshotIndexers() []index {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return append([]index(nil), p.indexers...)
}

func (p *Provider) cacheSyncTimeout() time.Duration {
	if p.opts.CacheSyncTimeout > 0 {
		return p.opts.CacheSyncTimeout
	}

	return defaultCacheSyncTimeout
}

func waitForRemoteCacheSync(ctx context.Context, cl cluster.Cluster, timeout time.Duration, startErr <-chan error) (bool, error) {
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	synced := make(chan bool, 1)

	go func() {
		synced <- cl.GetCache().WaitForCacheSync(syncCtx)
	}()

	select {
	case ok := <-synced:
		select {
		case err := <-startErr:
			if err == nil {
				return false, nil
			}

			return false, err
		default:
		}

		return ok, nil
	case err := <-startErr:
		if err == nil {
			return false, nil
		}

		return false, err
	case <-syncCtx.Done():
		return false, fmt.Errorf("wait for remote cache sync: %w", syncCtx.Err())
	}
}

type clusterRunState struct {
	lock sync.Mutex
	err  error
}

func (s *clusterRunState) markStopped(err error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.err == nil {
		s.err = err
	}
}

func (s *clusterRunState) errIfStopped() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	return s.err
}

func (p *Provider) publishCluster(key multicluster.ClusterName, cl cluster.Cluster, cancel context.CancelFunc, cfg *rest.Config) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.clusters[key] = cl
	p.cancelFns[key] = cancel
	p.kubeconfigs[key] = cloneComparableRestConfig(cfg)
}

// IndexField registers a cache field indexer on all current and future clusters.
func (p *Provider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.opsLock.Lock()
	defer p.opsLock.Unlock()

	p.lock.Lock()

	p.indexers = append(p.indexers, index{object: obj, field: field, extractValue: extractValue})

	clusters := make(map[multicluster.ClusterName]cluster.Cluster, len(p.clusters))
	for clusterProfileName, cl := range p.clusters {
		clusters[clusterProfileName] = cl
	}

	p.lock.Unlock()

	for clusterProfileName, cl := range clusters {
		if err := cl.GetCache().IndexField(ctx, obj, field, extractValue); err != nil {
			return fmt.Errorf("index field %q on ClusterProfile %q: %w", field, clusterProfileName, err)
		}
	}

	return nil
}

// restConfigEqual compares the connectivity-relevant fields of two rest.Config values.
// reflect.DeepEqual would always return false when any function pointer (WrapTransport,
// Dial, Proxy, etc.) is set, even on identical configs, causing pointless cluster
// re-engagement (cache resync = full cluster re-list).
func restConfigEqual(a, b *rest.Config) bool {
	if a == nil || b == nil {
		return a == b
	}

	if a.Host != b.Host || a.APIPath != b.APIPath {
		return false
	}

	if !restAuthEqual(a, b) {
		return false
	}

	if !restTLSEqual(a, b) {
		return false
	}

	if !restImpersonationEqual(a.Impersonate, b.Impersonate) {
		return false
	}

	if !restAuthProviderEqual(a.AuthProvider, b.AuthProvider) {
		return false
	}

	if !restExecProviderEqual(a.ExecProvider, b.ExecProvider) {
		return false
	}

	if !restTransportHooksEqual(a, b) {
		return false
	}

	return restProxyEqual(a, b)
}

func buildConfigFromClusterProfile(accessConfig *access.Config, profile *clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
	if accessConfig == nil {
		return nil, fmt.Errorf("ClusterProfile access config is nil")
	}

	cfg, err := remoteirsa.CloneAccessConfig(accessConfig).BuildConfigFromCP(profile)
	if err != nil {
		return nil, fmt.Errorf("build kubeconfig from ClusterProfile: %w", err)
	}

	return cfg, nil
}

func cloneStringSlicesMap(src map[string][]string) map[string][]string {
	if src == nil {
		return nil
	}

	dst := make(map[string][]string, len(src))
	for k, v := range src {
		dst[k] = slices.Clone(v)
	}

	return dst
}

func cloneComparableRestConfig(cfg *rest.Config) *rest.Config {
	if cfg == nil {
		return nil
	}

	clone := *cfg
	clone.CertData = slices.Clone(cfg.CertData)
	clone.KeyData = slices.Clone(cfg.KeyData)
	clone.CAData = slices.Clone(cfg.CAData)
	clone.NextProtos = slices.Clone(cfg.NextProtos)
	clone.Impersonate.Groups = slices.Clone(cfg.Impersonate.Groups)
	clone.Impersonate.Extra = cloneStringSlicesMap(cfg.Impersonate.Extra)

	if cfg.AuthProvider != nil {
		clone.AuthProvider = &clientcmdapi.AuthProviderConfig{
			Name:   cfg.AuthProvider.Name,
			Config: maps.Clone(cfg.AuthProvider.Config),
		}
	}

	if cfg.ExecProvider != nil {
		clone.ExecProvider = cfg.ExecProvider.DeepCopy()
	}

	return &clone
}

func restAuthEqual(a, b *rest.Config) bool {
	return a.Username == b.Username &&
		a.Password == b.Password &&
		a.BearerToken == b.BearerToken &&
		a.BearerTokenFile == b.BearerTokenFile
}

func restTLSEqual(a, b *rest.Config) bool {
	return a.Insecure == b.Insecure &&
		a.ServerName == b.ServerName &&
		a.CertFile == b.CertFile &&
		a.KeyFile == b.KeyFile &&
		a.CAFile == b.CAFile &&
		bytes.Equal(a.CertData, b.CertData) &&
		bytes.Equal(a.KeyData, b.KeyData) &&
		bytes.Equal(a.CAData, b.CAData) &&
		slices.Equal(a.NextProtos, b.NextProtos)
}

func restImpersonationEqual(a, b rest.ImpersonationConfig) bool {
	return a.UserName == b.UserName &&
		a.UID == b.UID &&
		slices.Equal(a.Groups, b.Groups) &&
		maps.EqualFunc(a.Extra, b.Extra, slices.Equal[[]string])
}

func restAuthProviderEqual(a, b *clientcmdapi.AuthProviderConfig) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Name == b.Name && maps.Equal(a.Config, b.Config)
}

func restExecProviderEqual(a, b *clientcmdapi.ExecConfig) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.APIVersion == b.APIVersion &&
		a.Command == b.Command &&
		slices.Equal(a.Args, b.Args) &&
		execEnvEqual(a.Env, b.Env) &&
		a.InstallHint == b.InstallHint &&
		a.ProvideClusterInfo == b.ProvideClusterInfo &&
		apiequality.Semantic.DeepEqual(a.Config, b.Config) &&
		a.InteractiveMode == b.InteractiveMode &&
		a.StdinUnavailable == b.StdinUnavailable &&
		a.StdinUnavailableMessage == b.StdinUnavailableMessage &&
		execPluginPolicyEqual(a.PluginPolicy, b.PluginPolicy)
}

func execEnvEqual(a, b []clientcmdapi.ExecEnvVar) bool {
	return maps.Equal(execEnvByName(a), execEnvByName(b))
}

func execEnvByName(env []clientcmdapi.ExecEnvVar) map[string]string {
	if len(env) == 0 {
		return nil
	}

	byName := make(map[string]string, len(env))
	for _, variable := range env {
		byName[variable.Name] = variable.Value
	}

	return byName
}

func execPluginPolicyEqual(a, b clientcmdapi.PluginPolicy) bool {
	return a.PolicyType == b.PolicyType &&
		slices.EqualFunc(a.Allowlist, b.Allowlist, func(a, b clientcmdapi.AllowlistEntry) bool {
			return a.Name == b.Name
		})
}

func restTransportHooksEqual(a, b *rest.Config) bool {
	return (a.Transport == nil) == (b.Transport == nil) &&
		(a.WrapTransport == nil) == (b.WrapTransport == nil) &&
		(a.Dial == nil) == (b.Dial == nil)
}

func restProxyEqual(a, b *rest.Config) bool {
	if a.Proxy == nil || b.Proxy == nil {
		return a.Proxy == nil && b.Proxy == nil
	}

	probeURL := proxyProbeURL(a.Host)

	return proxyResult(a.Proxy, probeURL) == proxyResult(b.Proxy, probeURL)
}

type proxyComparisonResult struct {
	url      string
	err      string
	hasError bool
}

func proxyResult(proxy func(*http.Request) (*url.URL, error), rawURL string) proxyComparisonResult {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return proxyComparisonResult{err: err.Error(), hasError: true}
	}

	proxyURL, err := proxy(req)

	result := proxyComparisonResult{}
	if proxyURL != nil {
		result.url = proxyURL.String()
	}

	if err != nil {
		result.err = err.Error()
		result.hasError = true
	}

	return result
}

func proxyProbeURL(host string) string {
	if host == "" {
		return "https://kubernetes.default.svc"
	}

	parsed, err := url.Parse(host)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://kubernetes.default.svc"
	}

	return parsed.String()
}

func (p *Provider) rememberProfileKey(requestKey, logicalKey multicluster.ClusterName) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if existing, ok := p.profileKeys[requestKey]; ok && existing != logicalKey {
		delete(p.profileKeys, requestKey)
		p.disengageLogicalIfUnreferencedLocked(existing)
	}

	p.profileKeys[requestKey] = logicalKey
}

func (p *Provider) disengageProfile(requestKey, fallbackKey multicluster.ClusterName) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// requestKey, the OCM-mapped logicalKey, and the fallback key can collide
	// (especially under the conventional Namespace==Name OCM convention) and a
	// separate ClusterProfile can already use this key as its own logicalKey
	// (two ClusterProfiles sharing an `open-cluster-management.io/cluster-name`
	// label map to the same logical remote cluster). Dedup per call AND skip
	// any key still referenced by a surviving profileKeys entry, otherwise a
	// duplicate ClusterProfile deletion cancels the shared cluster context.
	seen := map[multicluster.ClusterName]struct{}{}
	disengage := func(key multicluster.ClusterName) {
		if _, dup := seen[key]; dup {
			return
		}

		seen[key] = struct{}{}
		p.disengageLogicalIfUnreferencedLocked(key)
	}

	if logicalKey, ok := p.profileKeys[requestKey]; ok {
		delete(p.profileKeys, requestKey)
		disengage(logicalKey)
	}

	disengage(requestKey)
	disengage(fallbackKey)
}

// disengageLogicalIfUnreferencedLocked disengages key only when no remaining
// requestKey in profileKeys maps to it. The caller must hold p.lock.
func (p *Provider) disengageLogicalIfUnreferencedLocked(key multicluster.ClusterName) {
	for _, mapped := range p.profileKeys {
		if mapped == key {
			return
		}
	}

	p.disengageLocked(key)
}

func (p *Provider) disengageLocked(key multicluster.ClusterName) {
	if cancel, ok := p.cancelFns[key]; ok {
		cancel()
	}

	delete(p.clusters, key)
	delete(p.cancelFns, key)
	delete(p.kubeconfigs, key)
}

func (p *Provider) disengageIfCurrent(key multicluster.ClusterName, cl cluster.Cluster) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.clusters[key] != cl {
		return
	}

	p.disengageLocked(key)
}

func (p *Provider) isCurrentCluster(key multicluster.ClusterName, cl cluster.Cluster) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.clusters[key] == cl
}
