// Package inventory resolves ClusterProfile resources and manages remote clusters.
package inventory

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
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
)

var _ multicluster.Provider = &Provider{}

// Options customizes ClusterProfile-backed cluster creation.
type Options struct {
	ClusterOptions []cluster.Option
	NewCluster     func(ctx context.Context, profile *clusterinventoryv1alpha1.ClusterProfile, cfg *rest.Config, opts ...cluster.Option) (cluster.Cluster, error)
	IsReady        func(ctx context.Context, profile *clusterinventoryv1alpha1.ClusterProfile) bool
}

// Provider adapts ClusterProfile inventory into multicluster-runtime clusters.
type Provider struct {
	opts         Options
	accessConfig *access.Config
	log          logr.Logger
	client       client.Client
	lock         sync.RWMutex
	mcMgr        mcmanager.Manager
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
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	cfg, err := p.accessConfig.BuildConfigFromCP(profile)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("build kubeconfig for ClusterProfile %s: %w", requestKey, err)
	}

	p.rememberProfileKey(requestKey, key)

	return p.reconcileCluster(ctx, key, logger, profile, cfg)
}

func (p *Provider) reconcileCluster(ctx context.Context, key multicluster.ClusterName, logger logr.Logger, profile *clusterinventoryv1alpha1.ClusterProfile, cfg *rest.Config) (reconcile.Result, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.keepExistingCluster(key, cfg) {
		return reconcile.Result{}, nil
	}

	cl, err := p.opts.NewCluster(ctx, profile, cfg, p.opts.ClusterOptions...)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("create cluster for ClusterProfile %s: %w", key, err)
	}

	for _, idx := range p.indexers {
		if err := cl.GetCache().IndexField(ctx, idx.object, idx.field, idx.extractValue); err != nil {
			return reconcile.Result{}, fmt.Errorf("index field %q for ClusterProfile %s: %w", idx.field, key, err)
		}
	}

	clusterCtx, cancel := context.WithCancel(ctx)

	go func() {
		if err := cl.Start(clusterCtx); err != nil {
			logger.Error(err, "failed to start remote cluster")
		}
	}()

	if !cl.GetCache().WaitForCacheSync(ctx) {
		cancel()

		return reconcile.Result{}, fmt.Errorf("sync cache for ClusterProfile %s", key)
	}

	p.clusters[key] = cl
	p.cancelFns[key] = cancel
	p.kubeconfigs[key] = cfg

	if err := p.mcMgr.Engage(clusterCtx, key, cl); err != nil {
		p.disengageLocked(key)

		return reconcile.Result{}, fmt.Errorf("engage ClusterProfile %s: %w", key, err)
	}

	return reconcile.Result{}, nil
}

func (p *Provider) keepExistingCluster(key multicluster.ClusterName, cfg *rest.Config) bool {
	if _, ok := p.clusters[key]; !ok {
		return false
	}

	if p.kubeconfigs[key] != nil && restConfigEqual(p.kubeconfigs[key], cfg) {
		return true
	}

	p.disengageLocked(key)

	return false
}

// IndexField registers a cache field indexer on all current and future clusters.
func (p *Provider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.indexers = append(p.indexers, index{object: obj, field: field, extractValue: extractValue})
	for clusterProfileName, cl := range p.clusters {
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

	return a.Impersonate.UserName == b.Impersonate.UserName
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
		bytes.Equal(a.CAData, b.CAData)
}

func (p *Provider) rememberProfileKey(requestKey, logicalKey multicluster.ClusterName) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if existing, ok := p.profileKeys[requestKey]; ok && existing != logicalKey {
		p.disengageLocked(existing)
	}

	p.profileKeys[requestKey] = logicalKey
}

func (p *Provider) disengageProfile(requestKey, fallbackKey multicluster.ClusterName) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Deduplicate keys so disengageLocked is invoked at most once per cluster.
	// requestKey, the OCM-mapped logicalKey, and the fallback key can collide
	// (especially under the conventional Namespace==Name OCM convention);
	// without dedup we'd cancel the same context multiple times and pay
	// repeated map deletes.
	seen := map[multicluster.ClusterName]struct{}{}
	disengage := func(key multicluster.ClusterName) {
		if _, dup := seen[key]; dup {
			return
		}

		seen[key] = struct{}{}
		p.disengageLocked(key)
	}

	if logicalKey, ok := p.profileKeys[requestKey]; ok {
		disengage(logicalKey)
		delete(p.profileKeys, requestKey)
	}

	disengage(requestKey)
	disengage(fallbackKey)
}

func (p *Provider) disengageLocked(key multicluster.ClusterName) {
	if cancel, ok := p.cancelFns[key]; ok {
		cancel()
	}

	delete(p.clusters, key)
	delete(p.cancelFns, key)
	delete(p.kubeconfigs, key)
}
