// Package main starts the aws-workload-identity-operator manager.
package main

import (
	"context"
	"flag"
	"fmt"
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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/events"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
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
	s3UsePathStyle             bool
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
		Metrics:                metricsserver.Options{BindAddress: opts.metricsAddr},
		HealthProbeBindAddress: opts.probeAddr,
		LeaderElection:         opts.leaderElect,
		LeaderElectionID:       "aws-workload-identity-operator",
	})
	if err != nil {
		return nil, fmt.Errorf("create multicluster manager: %w", err)
	}

	return mgr, nil
}

func parseFlags() managerOptions {
	opts := managerOptions{}

	flag.StringVar(&opts.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to.")
	flag.StringVar(&opts.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&opts.leaderElect, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&opts.clusterProfileProviderFile, "clusterprofile-provider-file", "clusterprofile-provider-file.json", "Path to the JSON config file describing ClusterProfile access providers.")
	flag.StringVar(&opts.podIdentityWebhookImage, "pod-identity-webhook-image", controller.DefaultPodIdentityWebhookImage, "Remote pod identity webhook image.")
	flag.StringVar(&opts.awsEndpointURL, "aws-endpoint-url", "", "The AWS endpoint URL the operator manager will use for direct AWS API calls.")
	flag.BoolVar(&opts.allowUnsafeAWSEndpointURL, "allow-unsafe-aws-endpoint-urls", false, "Allow an unsafe AWS endpoint URL over HTTP.")
	flag.BoolVar(&opts.s3UsePathStyle, "s3-use-path-style", false, "Use path-style S3 requests for self-hosted OIDC object publishing.")
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

func withWebhookRuntimeCacheScope(options *cluster.Options) {
	if options.Cache.ByObject == nil {
		options.Cache.ByObject = make(map[client.Object]cache.ByObject)
	}

	mergeCacheByObjectByGVK(options.Cache.ByObject, controller.SelfHostedWebhookRuntimeCacheByObject(), clusterOptionScheme(options))

	if options.Client.Cache == nil {
		options.Client.Cache = &client.CacheOptions{}
	}

	options.Client.Cache.DisableFor = appendDisableForByGVK(options.Client.Cache.DisableFor, controller.SelfHostedWebhookRuntimeUncachedReadObjects(), clusterOptionScheme(options))
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
			if !cacheByObjectAlreadyScoped(dst[dstObj], srcByObject) {
				panic(fmt.Sprintf("cache ByObject for %s is already configured; cannot add webhook runtime selector safely", srcGVK))
			}

			continue
		}

		dst[srcObj] = srcByObject
		dstByGVK[srcGVK] = srcObj
	}
}

func cacheByObjectAlreadyScoped(existing, incoming cache.ByObject) bool {
	if !sameLabelSelector(existing.Label, incoming.Label) {
		return false
	}

	for _, config := range existing.Namespaces {
		if config.LabelSelector != nil && !sameLabelSelector(config.LabelSelector, incoming.Label) {
			return false
		}
	}

	return true
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
	recorder := localMgr.GetEventRecorder("aws-workload-identity-operator")
	resolver := inventory.Resolver{Client: localClient}
	roleEnqueueCh := make(chan crevent.TypedGenericEvent[*identityv1.AWSServiceAccountRole], remoteEventChannelBuffer)
	runtimeEventCh := make(chan crevent.TypedGenericEvent[*identityv1.AWSWorkloadIdentityConfig], remoteEventChannelBuffer)

	if err := localMgr.GetFieldIndexer().IndexField(context.Background(), &identityv1.AWSServiceAccountRole{}, controller.IndexRoleByServiceAccount, controller.IndexAWSServiceAccountRoleBySA); err != nil {
		return fmt.Errorf("register AWSServiceAccountRole field index: %w", err)
	}

	if err := localMgr.GetFieldIndexer().IndexField(context.Background(), &identityv1.AWSWorkloadIdentityConfig{}, controller.IndexConfigByResolvedCluster, controller.IndexAWSWorkloadIdentityConfigByResolvedCluster); err != nil {
		return fmt.Errorf("register AWSWorkloadIdentityConfig resolved cluster field index: %w", err)
	}

	if err := setupLocalControllers(mcMgr, localMgr, localClient, recorder, roleEnqueueCh, runtimeEventCh, opts); err != nil {
		return err
	}

	if err := controller.SetupSelfHostedRoleEnqueueController(mcMgr, localClient, roleEnqueueCh); err != nil {
		return fmt.Errorf("create self-hosted role enqueue controller: %w", err)
	}

	if err := controller.SetupSelfHostedServiceAccountController(mcMgr, localClient, resolver); err != nil {
		return fmt.Errorf("create self-hosted ServiceAccount controller: %w", err)
	}

	if err := controller.SetupSelfHostedWebhookRuntimeController(mcMgr, localClient, runtimeEventCh); err != nil {
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
		SelfHostedIssuerPublisherFactory: newSelfHostedIssuerPublisherFactory(opts.awsEndpointURL, opts.s3UsePathStyle),
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

	return nil
}

func newSelfHostedIssuerPublisherFactory(endpointURL string, usePathStyle bool) controller.SelfHostedIssuerPublisherFactory {
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

				options.UsePathStyle = usePathStyle
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

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("set up ready check: %w", err)
	}

	return nil
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
	server.Register("/validate-aws-identity-appthrust-io-v1alpha1-awsworkloadidentityconfig",
		admission.WithValidator[*identityv1.AWSWorkloadIdentityConfig](scheme, controller.ConfigValidator{Client: mgr.GetClient()}))
	server.Register("/validate-aws-identity-appthrust-io-v1alpha1-awsserviceaccountrole",
		admission.WithValidator[*identityv1.AWSServiceAccountRole](scheme, controller.RoleValidator{Client: mgr.GetClient()}))
	server.Register("/validate-aws-identity-appthrust-io-v1alpha1-awsworkloadidentityoperatorconfig",
		admission.WithValidator[*identityv1.AWSWorkloadIdentityOperatorConfig](scheme, controller.OperatorConfigValidator{}))
}
