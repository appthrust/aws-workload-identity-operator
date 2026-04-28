// Package main starts the aws-workload-identity-operator manager.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	eksv1alpha1 "github.com/aws-controllers-k8s/eks-controller/apis/v1alpha1"
	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	s3v1alpha1 "github.com/aws-controllers-k8s/s3-controller/apis/v1alpha1"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awssdkconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
	"github.com/appthrust/aws-workload-identity-operator/internal/controller"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/logging"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// remoteEventChannelBuffer sizes the buffered channels that bridge remote
// drift events into the local Reconcile loops. Channel-full skips are
// recorded as awio_remote_delivery_total{reason="channel_full"}; if that
// rate climbs, raise this value (or scale up controller workers) before
// hand-tuning the producers.
const remoteEventChannelBuffer = 1024

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(identityv1.AddToScheme(scheme))
	utilruntime.Must(clusterv1beta1.Install(scheme))
	utilruntime.Must(clusterinventoryv1alpha1.AddToScheme(scheme))
	utilruntime.Must(iamv1alpha1.AddToScheme(scheme))
	utilruntime.Must(s3v1alpha1.AddToScheme(scheme))
	utilruntime.Must(eksv1alpha1.AddToScheme(scheme))
}

type managerOptions struct {
	metricsAddr                string
	probeAddr                  string
	clusterProfileProviderFile string
	podIdentityWebhookImage    string
	awsEndpointURL             string
	logLevel                   string
	logExporter                string
	logResourceAttributes      string
	leaderElect                bool
	logAddSource               bool
	allowUnsafeAWSEndpointURL  bool
	maxConcurrentReconciles    int
}

func main() {
	os.Exit(run())
}

func run() int {
	opts := parseFlags()

	if err := validateOptions(&opts); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return 1
	}

	shutdownLogger, err := configureLogger(&opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return 1
	}
	defer shutdownLogging(shutdownLogger)

	provider, err := inventory.NewProviderFromFile(opts.clusterProfileProviderFile, inventory.Options{
		ClusterOptions: []cluster.Option{withScheme, withWebhookRuntimeCacheScope},
	})
	if err != nil {
		setupLog.Error(err, "unable to create Cluster Inventory provider")

		return 1
	}

	mcMgr, err := newMulticlusterManager(provider, &opts)
	if err != nil {
		setupLog.Error(err, "unable to create multicluster manager")

		return 1
	}

	if err := provider.SetupWithManager(mcMgr); err != nil {
		setupLog.Error(err, "unable to set up Cluster Inventory provider")

		return 1
	}

	if err := setupControllers(mcMgr, &opts); err != nil {
		setupLog.Error(err, "unable to set up controllers")

		return 1
	}

	setupLog.Info("starting manager")

	if err := mcMgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")

		return 1
	}

	return 0
}

func newMulticlusterManager(provider *inventory.Provider, opts *managerOptions) (mcmanager.Manager, error) {
	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), provider, mcmanager.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions(opts),
		HealthProbeBindAddress: opts.probeAddr,
		LeaderElection:         opts.leaderElect,
		LeaderElectionID:       identityv1.ManagedByValue,
		Cache: cache.Options{
			ByObject: localCacheByObject(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create multicluster manager: %w", err)
	}

	return mgr, nil
}

func metricsServerOptions(opts *managerOptions) metricsserver.Options {
	return metricsserver.Options{
		BindAddress:    opts.metricsAddr,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}
}

func parseFlags() managerOptions {
	opts := managerOptions{}

	flag.StringVar(&opts.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to.")
	flag.StringVar(&opts.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&opts.leaderElect, "leader-elect", false, "Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&opts.clusterProfileProviderFile, "clusterprofile-provider-file", "clusterprofile-provider-file.json", "Path to the JSON config file describing ClusterProfile access providers.")
	flag.StringVar(&opts.podIdentityWebhookImage, "pod-identity-webhook-image", controller.DefaultPodIdentityWebhookImage, "Remote pod identity webhook image.")
	flag.StringVar(&opts.awsEndpointURL, "aws-endpoint-url", "", "The AWS endpoint URL the operator manager will use for direct AWS API calls.")
	flag.BoolVar(&opts.allowUnsafeAWSEndpointURL, "allow-unsafe-aws-endpoint-urls", false, "Allow an unsafe AWS endpoint URL over HTTP.")
	flag.StringVar(&opts.logLevel, "log-level", logging.DefaultLevel, "Minimum log level: trace, debug, info, warn, or error.")
	flag.BoolVar(&opts.logAddSource, "log-add-source", false, "Include source file, function, and line attributes in log records.")
	flag.StringVar(&opts.logExporter, "log-exporter", "", "Log exporter: otlp, console, or none. Empty honors OTEL_LOGS_EXPORTER, then defaults to otlp.")
	flag.StringVar(&opts.logResourceAttributes, "log-resource-attributes", "", "Comma-separated OpenTelemetry resource attributes.")
	flag.IntVar(&opts.maxConcurrentReconciles, "max-concurrent-reconciles", 4, "Maximum number of concurrent reconciles per controller.")
	flag.Parse()

	return opts
}

func validateOptions(opts *managerOptions) error {
	if opts.awsEndpointURL == "" {
		return nil
	}

	endpoint, err := url.Parse(opts.awsEndpointURL)
	if err != nil {
		return fmt.Errorf("invalid --aws-endpoint-url: %w", err)
	}

	if !endpoint.IsAbs() || endpoint.Hostname() == "" {
		return fmt.Errorf("--aws-endpoint-url must be an absolute URL with a host")
	}

	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return fmt.Errorf("--aws-endpoint-url scheme must be http or https")
	}

	if !opts.allowUnsafeAWSEndpointURL && endpoint.Scheme == "http" {
		return fmt.Errorf("using an unsafe AWS endpoint URL is not allowed")
	}

	return nil
}

func configureLogger(opts *managerOptions) (logging.ShutdownFunc, error) {
	resourceAttributes, err := logging.ParseResourceAttributesStrict(opts.logResourceAttributes)
	if err != nil {
		return nil, fmt.Errorf("invalid --log-resource-attributes: %w", err)
	}

	_, logger, shutdownLogger, err := logging.NewLogger(context.Background(), &logging.Options{
		Level:                 opts.logLevel,
		AddSource:             opts.logAddSource,
		Exporter:              opts.logExporter,
		ResourceAttributesRaw: opts.logResourceAttributes,
		ResourceAttributes:    resourceAttributes,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to configure logger: %w", err)
	}

	ctrl.SetLogger(logger)

	setupLog = ctrl.Log.WithName("setup")

	return shutdownLogger, nil
}

func withScheme(options *cluster.Options) {
	options.Scheme = scheme
}

// localCacheByObject builds the local multicluster manager's per-type cache
// options. Secret stripping and Namespace managed-fields stripping are both
// required for the local manager's cluster-wide informers; they live in
// separate factories so each can be tested independently.
func localCacheByObject() map[client.Object]cache.ByObject {
	result := make(map[client.Object]cache.ByObject)
	mergeCacheByObjectByGVK(result, controller.LocalSecretCacheByObject(), scheme)
	mergeCacheByObjectByGVK(result, controller.LocalNamespaceCacheByObject(), scheme)

	return result
}

func withWebhookRuntimeCacheScope(options *cluster.Options) {
	if options.Cache.ByObject == nil {
		options.Cache.ByObject = make(map[client.Object]cache.ByObject)
	}

	mergeCacheByObjectByGVK(options.Cache.ByObject, controller.SelfHostedWebhookRuntimeCacheByObject(), clusterOptionScheme(options))
	mergeCacheByObjectByGVK(options.Cache.ByObject, controller.RemoteServiceAccountCacheByObject(), clusterOptionScheme(options))

	if options.Client.Cache == nil {
		options.Client.Cache = &client.CacheOptions{}
	}

	options.Client.Cache.DisableFor = appendDisableForByGVK(options.Client.Cache.DisableFor, controller.SelfHostedWebhookRuntimeUncachedReadObjects(), clusterOptionScheme(options))
	options.Client.Cache.DisableFor = appendDisableForByGVK(options.Client.Cache.DisableFor, controller.RemoteServiceAccountUncachedReadObjects(), clusterOptionScheme(options))
}

func clusterOptionScheme(options *cluster.Options) *runtime.Scheme {
	if options.Scheme != nil {
		return options.Scheme
	}

	return scheme
}

func mergeCacheByObjectByGVK(dst, src map[client.Object]cache.ByObject, scheme *runtime.Scheme) {
	dstByGVK := make(map[schema.GroupVersionKind]client.Object, len(dst))
	for dstObj := range dst {
		dstGVK := mustGVKForObject(dstObj, scheme)
		if _, ok := dstByGVK[dstGVK]; ok {
			panic(fmt.Sprintf("duplicate cache ByObject entries for %s", dstGVK))
		}

		dstByGVK[dstGVK] = dstObj
	}

	for srcObj, srcByObject := range src {
		srcGVK := mustGVKForObject(srcObj, scheme)
		if dstObj, ok := dstByGVK[srcGVK]; ok {
			merged, ok := mergeCacheByObjectOptions(dst[dstObj], srcByObject)
			if !ok {
				panic(fmt.Sprintf("cache ByObject for %s is already configured incompatibly; cannot merge required remote cache options safely", srcGVK))
			}

			dst[dstObj] = merged

			continue
		}

		dst[srcObj] = srcByObject
		dstByGVK[srcGVK] = srcObj
	}
}

func mergeCacheByObjectOptions(existing, incoming cache.ByObject) (cache.ByObject, bool) {
	if !cacheByObjectCompatible(existing, incoming) {
		return cache.ByObject{}, false
	}

	existing.Transform = composeCacheTransforms(existing.Transform, incoming.Transform)

	if existing.Namespaces == nil && incoming.Namespaces != nil {
		existing.Namespaces = incoming.Namespaces
	}

	if existing.UnsafeDisableDeepCopy == nil {
		existing.UnsafeDisableDeepCopy = incoming.UnsafeDisableDeepCopy
	}

	if existing.EnableWatchBookmarks == nil {
		existing.EnableWatchBookmarks = incoming.EnableWatchBookmarks
	}

	if existing.SyncPeriod == nil {
		existing.SyncPeriod = incoming.SyncPeriod
	}

	return existing, true
}

func cacheByObjectCompatible(existing, incoming cache.ByObject) bool {
	if !sameLabelSelector(existing.Label, incoming.Label) {
		return false
	}

	if !sameFieldSelector(existing.Field, incoming.Field) {
		return false
	}

	return sameNamespaceCacheScope(existing.Namespaces, incoming.Namespaces)
}

func sameNamespaceCacheScope(a, b map[string]cache.Config) bool {
	if len(a) != len(b) {
		return false
	}

	for namespace, aConfig := range a {
		bConfig, ok := b[namespace]
		if !ok {
			return false
		}

		if !sameLabelSelector(aConfig.LabelSelector, bConfig.LabelSelector) {
			return false
		}

		if !sameFieldSelector(aConfig.FieldSelector, bConfig.FieldSelector) {
			return false
		}
	}

	return true
}

func composeCacheTransforms(first, second toolscache.TransformFunc) toolscache.TransformFunc {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return func(in any) (any, error) {
			out, err := first(in)
			if err != nil {
				return nil, err
			}

			return second(out)
		}
	}
}

func sameLabelSelector(a, b labels.Selector) bool {
	if a == nil || a.Empty() {
		return b == nil || b.Empty()
	}

	if b == nil || b.Empty() {
		return false
	}

	return a.String() == b.String()
}

func sameFieldSelector(a, b fields.Selector) bool {
	if a == nil || a.Empty() {
		return b == nil || b.Empty()
	}

	if b == nil || b.Empty() {
		return false
	}

	return a.String() == b.String()
}

func appendDisableForByGVK(dst, src []client.Object, scheme *runtime.Scheme) []client.Object {
	seen := make(map[schema.GroupVersionKind]struct{}, len(dst)+len(src))
	for _, obj := range dst {
		seen[mustGVKForObject(obj, scheme)] = struct{}{}
	}

	for _, obj := range src {
		gvk := mustGVKForObject(obj, scheme)
		if _, ok := seen[gvk]; ok {
			continue
		}

		dst = append(dst, obj)
		seen[gvk] = struct{}{}
	}

	return dst
}

func mustGVKForObject(obj client.Object, scheme *runtime.Scheme) schema.GroupVersionKind {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		panic(fmt.Sprintf("resolve GVK for %T: %v", obj, err))
	}

	return gvk
}

func setupControllers(mcMgr mcmanager.Manager, opts *managerOptions) error {
	localMgr := mcMgr.GetLocalManager()
	localClient := localMgr.GetClient()
	recorder := localMgr.GetEventRecorder(identityv1.ManagedByValue)
	resolver := inventory.Resolver{Client: localClient}
	roleEnqueueCh := make(chan crevent.TypedGenericEvent[*identityv1.AWSServiceAccountRole], remoteEventChannelBuffer)
	runtimeEventCh := make(chan crevent.TypedGenericEvent[*identityv1.AWSWorkloadIdentityConfig], remoteEventChannelBuffer)

	if err := localMgr.GetFieldIndexer().IndexField(context.Background(), &identityv1.AWSServiceAccountRole{}, controller.IndexRoleByServiceAccount, controller.IndexAWSServiceAccountRoleBySA); err != nil {
		return fmt.Errorf("register AWSServiceAccountRole field index: %w", err)
	}

	if err := localMgr.GetFieldIndexer().IndexField(context.Background(), &identityv1.AWSServiceAccountRole{}, controller.IndexRoleByReplicaSetUID, controller.IndexAWSServiceAccountRoleByReplicaSetUID); err != nil {
		return fmt.Errorf("register AWSServiceAccountRole ReplicaSet UID field index: %w", err)
	}

	if err := localMgr.GetFieldIndexer().IndexField(context.Background(), &identityv1.AWSServiceAccountRoleReplicaSet{}, controller.IndexReplicaSetByPlacementRef, controller.IndexAWSServiceAccountRoleReplicaSetByPlacementRef); err != nil {
		return fmt.Errorf("register AWSServiceAccountRoleReplicaSet placementRef field index: %w", err)
	}

	if err := localMgr.GetFieldIndexer().IndexField(context.Background(), &identityv1.AWSWorkloadIdentityConfig{}, controller.IndexConfigByResolvedCluster, controller.IndexAWSWorkloadIdentityConfigByResolvedCluster); err != nil {
		return fmt.Errorf("register AWSWorkloadIdentityConfig resolved cluster field index: %w", err)
	}

	if err := setupLocalControllers(mcMgr, localMgr, localClient, recorder, roleEnqueueCh, runtimeEventCh, opts); err != nil {
		return err
	}

	if err := (&controller.SelfHostedRoleEnqueueController{
		LocalClient:             localClient,
		RoleEvents:              roleEnqueueCh,
		MaxConcurrentReconciles: opts.maxConcurrentReconciles,
	}).SetupWithManager(mcMgr); err != nil {
		return fmt.Errorf("create self-hosted role enqueue controller: %w", err)
	}

	if err := (&controller.SelfHostedServiceAccountReconciler{
		LocalClient:             localClient,
		MCManager:               mcMgr,
		Resolver:                resolver,
		Recorder:                recorder,
		MaxConcurrentReconciles: opts.maxConcurrentReconciles,
	}).SetupWithManager(mcMgr); err != nil {
		return fmt.Errorf("create self-hosted ServiceAccount controller: %w", err)
	}

	if err := (&controller.SelfHostedWebhookRuntimeReconciler{
		LocalClient:             localClient,
		RuntimeEvents:           runtimeEventCh,
		MaxConcurrentReconciles: opts.maxConcurrentReconciles,
	}).SetupWithManager(mcMgr); err != nil {
		return fmt.Errorf("create self-hosted webhook runtime controller: %w", err)
	}

	registerWebhooks(localMgr)

	return setupHealthChecks(localMgr)
}

func setupLocalControllers(
	mcMgr mcmanager.Manager,
	mgr ctrl.Manager,
	localClient client.Client,
	recorder events.EventRecorder,
	roleEnqueueCh <-chan crevent.TypedGenericEvent[*identityv1.AWSServiceAccountRole],
	runtimeEventCh <-chan crevent.TypedGenericEvent[*identityv1.AWSWorkloadIdentityConfig],
	opts *managerOptions,
) error {
	if err := (&controller.AWSWorkloadIdentityConfigReconciler{
		Client:                           localClient,
		Scheme:                           mgr.GetScheme(),
		Recorder:                         recorder,
		SigningSecretReader:              mgr.GetAPIReader(),
		SelfHostedIssuerPublisherFactory: newSelfHostedIssuerPublisherFactory(opts.awsEndpointURL),
		MaxConcurrentReconciles:          opts.maxConcurrentReconciles,
		RuntimeEventChannel:              runtimeEventCh,
		MCManager:                        mcMgr,
		PodIdentityWebhookImage:          opts.podIdentityWebhookImage,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("create config controller: %w", err)
	}

	if err := (&controller.AWSServiceAccountRoleReconciler{
		Client:                  localClient,
		MCManager:               mcMgr,
		Scheme:                  mgr.GetScheme(),
		Recorder:                recorder,
		MaxConcurrentReconciles: opts.maxConcurrentReconciles,
		RoleEnqueueChannel:      roleEnqueueCh,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("create role controller: %w", err)
	}

	if err := (&controller.AWSServiceAccountRoleReplicaSetReconciler{
		Client:                  localClient,
		Scheme:                  mgr.GetScheme(),
		Recorder:                recorder,
		MaxConcurrentReconciles: opts.maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("create role ReplicaSet controller: %w", err)
	}

	return nil
}

func newSelfHostedIssuerPublisherFactory(endpointURL string) controller.SelfHostedIssuerPublisherFactory {
	var (
		mu         sync.RWMutex
		publishers = map[string]controller.SelfHostedIssuerPublisher{}
		group      singleflight.Group
	)

	return func(ctx context.Context, region string) (controller.SelfHostedIssuerPublisher, error) {
		mu.RLock()

		cached, ok := publishers[region]

		mu.RUnlock()

		if ok {
			return cached, nil
		}

		// singleflight collapses concurrent first-time loads for the same
		// region so LoadDefaultConfig (potential IMDS/STS round trips) runs
		// once per region.
		result, err, _ := group.Do(region, func() (any, error) {
			cfg, err := awssdkconfig.LoadDefaultConfig(ctx, awssdkconfig.WithRegion(region))
			if err != nil {
				return nil, fmt.Errorf("load AWS config for region %q: %w", region, err)
			}

			publisher := identityaws.NewS3OIDCIssuerPublisher(awss3.NewFromConfig(cfg, func(options *awss3.Options) {
				if endpointURL != "" {
					options.BaseEndpoint = awssdk.String(endpointURL)
				}
			}))

			mu.Lock()
			publishers[region] = publisher
			mu.Unlock()

			return publisher, nil
		})
		if err != nil {
			return nil, fmt.Errorf("load self-hosted issuer publisher for region %q: %w", region, err)
		}

		publisher, ok := result.(controller.SelfHostedIssuerPublisher)
		if !ok {
			return nil, fmt.Errorf("singleflight returned unexpected publisher type %T", result)
		}

		return publisher, nil
	}
}

func setupHealthChecks(mgr ctrl.Manager) error {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("set up health check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", mgr.GetWebhookServer().StartedChecker()); err != nil {
		return fmt.Errorf("set up ready check: %w", err)
	}

	if err := mgr.AddReadyzCheck("informer-cache-sync", cacheSyncReadyCheck(mgr.GetCache().WaitForCacheSync)); err != nil {
		return fmt.Errorf("set up informer cache ready check: %w", err)
	}

	return nil
}

// cacheSyncReadyCheck reports informer cache readiness for /readyz. Until
// the local manager's caches finish their initial list/watch, reconcilers
// can read empty or stale state, so the probe must hold at 503 to keep the
// pod out of rotation. Once the caches sync, waitForSync returns true
// immediately on each call, so the probe stops blocking.
func cacheSyncReadyCheck(waitForSync func(ctx context.Context) bool) healthz.Checker {
	return func(req *http.Request) error {
		if !waitForSync(req.Context()) {
			return fmt.Errorf("informer cache not synced")
		}

		return nil
	}
}

func shutdownLogging(shutdownLogger logging.ShutdownFunc) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := shutdownLogger(shutdownCtx); err != nil {
		setupLog.Error(err, "unable to shutdown logger")
	}
}

func registerWebhooks(mgr ctrl.Manager) {
	server := mgr.GetWebhookServer()
	// Validators read directly from the API server, not the informer cache, so
	// ordinary duplicate checks are not weakened by cache lag. Truly concurrent
	// creates can still race; the reconciler reports those conflicts in status.
	apiReader := mgr.GetAPIReader()
	server.Register("/validate-aws-identity-appthrust-io-v1alpha1-awsworkloadidentityconfig",
		admission.WithValidator[*identityv1.AWSWorkloadIdentityConfig](scheme, controller.ConfigValidator{Client: apiReader}))
	server.Register("/validate-aws-identity-appthrust-io-v1alpha1-awsserviceaccountrole",
		admission.WithValidator[*identityv1.AWSServiceAccountRole](scheme, controller.RoleValidator{Client: apiReader}))
	server.Register("/validate-aws-identity-appthrust-io-v1alpha1-awsserviceaccountrolereplicaset",
		admission.WithValidator[*identityv1.AWSServiceAccountRoleReplicaSet](scheme, controller.RoleReplicaSetValidator{}))
}
