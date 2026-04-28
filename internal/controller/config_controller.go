package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	s3v1alpha1 "github.com/aws-controllers-k8s/s3-controller/apis/v1alpha1"
	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
)

// AWSWorkloadIdentityConfigReconciler reconciles namespace identity config resources.
//
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsserviceaccountroles,verbs=get;list;watch
// +kubebuilder:rbac:groups=aws.identity.appthrust.io,resources=awsworkloadidentityoperatorconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=iam.services.k8s.aws,resources=openidconnectproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=s3.services.k8s.aws,resources=buckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type AWSWorkloadIdentityConfigReconciler struct {
	client.Client
	Scheme                           *runtime.Scheme
	Recorder                         events.EventRecorder
	Resolver                         inventory.Resolver
	SelfHostedIssuerPublisherFactory SelfHostedIssuerPublisherFactory
	MaxConcurrentReconciles          int
	RuntimeEventChannel              <-chan crevent.TypedGenericEvent[*identityv1.AWSWorkloadIdentityConfig]
	MCManager                        remoteClusterGetter
	PodIdentityWebhookImage          string
}

// SelfHostedIssuerPublisher writes public OIDC issuer objects for SelfHostedIRSA.
type SelfHostedIssuerPublisher interface {
	DeleteOIDCIssuer(ctx context.Context, bucket string) error
	PublishOIDCIssuer(ctx context.Context, bucket, issuerURL string, publicKeyPEM []byte, keyID string) error
}

// SelfHostedIssuerPublisherFactory returns a publisher for the requested AWS region.
type SelfHostedIssuerPublisherFactory func(ctx context.Context, region string) (SelfHostedIssuerPublisher, error)

// Reconcile provisions the OIDC issuer and self-hosted webhook runtime that
// back the namespace's identity config, or marks them not-required for
// EKSPodIdentity delivery.
func (r *AWSWorkloadIdentityConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	log := loggerForRequest(ctx, metrics.ControllerConfig, req)
	ctx = logf.IntoContext(ctx, log)
	log.V(1).Info("starting reconcile")

	defer func() {
		logReconcileEnd(log, result, reconcileErr)
	}()

	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		if ignored := client.IgnoreNotFound(err); ignored != nil {
			return ctrl.Result{}, fmt.Errorf("get AWSWorkloadIdentityConfig %s: %w", req.NamespacedName, ignored)
		}

		return ctrl.Result{}, nil
	}

	log = log.WithValues("awio.delivery.type", string(config.Spec.Type))
	ctx = logf.IntoContext(ctx, log)

	added, err := ensureFinalizer(ctx, r.Client, r.Recorder, log, config, identityv1.ConfigFinalizer)
	if err != nil {
		return ctrl.Result{}, err
	}

	if added {
		return ctrl.Result{Requeue: true}, nil
	}

	if !config.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, config)
	}

	return r.reconcileNormal(ctx, config)
}

func (r *AWSWorkloadIdentityConfigReconciler) reconcileNormal(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) (ctrl.Result, error) {
	beforeStatus := config.Status.DeepCopy()
	log := logf.FromContext(ctx)

	config.Status.ObservedGeneration = config.Generation

	operatorConfig, err := loadOperatorConfig(ctx, r.Client)
	if err != nil {
		failReady(&config.Status.Conditions, config.Generation, identityv1.ConditionOperatorConfigReady, identityv1.ReasonOperatorConfigUnavailable, err.Error())
		log.V(1).Info("operator configuration unavailable", "awio.operation", "load_operator_config")

		return ctrl.Result{RequeueAfter: transientRequeue}, r.patchConfigStatus(ctx, log, config, beforeStatus)
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionOperatorConfigReady, metav1.ConditionTrue, identityv1.ReasonReady, "operator configuration is valid")

	resolved, err := r.Resolver.Resolve(ctx, config.Namespace)
	if err != nil {
		failReady(&config.Status.Conditions, config.Generation, identityv1.ConditionClusterProfileResolved, identityv1.ReasonResolverError, err.Error())
		config.Status.ResolvedClusterName = ""

		if patchErr := r.patchConfigStatus(ctx, log, config, beforeStatus); patchErr != nil {
			log.Error(patchErr, "failed to patch status after inventory resolver error")
		}

		return ctrl.Result{}, fmt.Errorf("resolve inventory for namespace %q: %w", config.Namespace, err)
	}

	setClusterProfileResolvedCondition(log, config, &resolved)
	setResolvedClusterStatus(config, &resolved)

	var (
		result       ctrl.Result
		componentErr error
	)

	if config.Spec.Type == identityv1.DeliveryTypeSelfHostedIRSA {
		result, componentErr = r.reconcileSelfHostedComponents(ctx, log, config, operatorConfig, &resolved)
	} else {
		config.Status.ACKResources = nil
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonNotRequired, "EKSPodIdentity does not use a self-hosted issuer")
		resetSelfHostedStatus(config)
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, identityv1.ReasonNotRequired, "EKSPodIdentity does not use the self-hosted webhook runtime")
	}

	setConfigReadyCondition(config, &resolved)

	if err := r.patchConfigStatus(ctx, log, config, beforeStatus); err != nil {
		return ctrl.Result{}, err
	}

	return result, componentErr
}

// reconcileSelfHostedComponents runs the independent self-hosted issuer and
// webhook runtime paths against isolated status copies, then merges the fields
// each path owns back into config for the single final status patch.
func (r *AWSWorkloadIdentityConfigReconciler) reconcileSelfHostedComponents(
	ctx context.Context,
	log logr.Logger,
	config *identityv1.AWSWorkloadIdentityConfig,
	operatorConfig *identityv1.AWSWorkloadIdentityOperatorConfig,
	resolved *inventory.Resolution,
) (ctrl.Result, error) {
	issuerConfig := config.DeepCopy()
	runtimeConfig := config.DeepCopy()

	var (
		wg            sync.WaitGroup
		issuerErr     error
		runtimeErr    error
		runtimeResult ctrl.Result
	)

	wg.Add(2)

	go func() {
		defer wg.Done()

		if err := r.reconcileSelfHostedIssuer(ctx, issuerConfig); err != nil {
			setSelfHostedIssuerFailureCondition(issuerConfig, err)
			issuerErr = err
		}
	}()

	go func() {
		defer wg.Done()

		runtimeResult, runtimeErr = r.reconcileSelfHostedWebhookRuntime(ctx, log, runtimeConfig, operatorConfig, resolved)
	}()

	wg.Wait()

	mergeSelfHostedIssuerStatus(config, issuerConfig)
	mergeSelfHostedWebhookRuntimeStatus(config, runtimeConfig)

	return runtimeResult, errors.Join(issuerErr, runtimeErr)
}

func mergeSelfHostedIssuerStatus(dst, src *identityv1.AWSWorkloadIdentityConfig) {
	srcStatus := src.Status.DeepCopy()

	dst.Status.ACKResources = srcStatus.ACKResources
	dst.Status.BucketName = srcStatus.BucketName
	dst.Status.IssuerHostPath = srcStatus.IssuerHostPath
	dst.Status.OIDCProviderARN = srcStatus.OIDCProviderARN
	dst.Status.PublishedKeyID = srcStatus.PublishedKeyID

	copyConditionByType(&dst.Status.Conditions, src.Status.Conditions, identityv1.ConditionBucketReady)
	copyConditionByType(&dst.Status.Conditions, src.Status.Conditions, identityv1.ConditionOIDCObjectsPublished)
	copyConditionByType(&dst.Status.Conditions, src.Status.Conditions, identityv1.ConditionIAMProviderReady)
	copyConditionByType(&dst.Status.Conditions, src.Status.Conditions, identityv1.ConditionIssuerReady)
}

func mergeSelfHostedWebhookRuntimeStatus(dst, src *identityv1.AWSWorkloadIdentityConfig) {
	srcStatus := src.Status.DeepCopy()

	dst.Status.WebhookRuntimeNamespace = srcStatus.WebhookRuntimeNamespace
	dst.Status.WebhookRuntimeCertNotAfter = srcStatus.WebhookRuntimeCertNotAfter

	copyConditionByType(&dst.Status.Conditions, src.Status.Conditions, identityv1.ConditionWebhookRuntimeReady)
}

func copyConditionByType(dst *[]metav1.Condition, src []metav1.Condition, typ string) {
	cond := meta.FindStatusCondition(src, typ)
	if cond == nil {
		return
	}

	setCondition(dst, cond.ObservedGeneration, cond.Type, cond.Status, cond.Reason, cond.Message)
}

func setClusterProfileResolvedCondition(log logr.Logger, config *identityv1.AWSWorkloadIdentityConfig, resolved *inventory.Resolution) {
	if resolved.Ready {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionClusterProfileResolved, metav1.ConditionTrue, resolved.Reason, resolved.Message)

		return
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionClusterProfileResolved, metav1.ConditionFalse, resolved.Reason, resolved.Message)
	log.V(1).Info("waiting for inventory resolution", "awio.condition.reason", resolved.Reason)
}

func setResolvedClusterStatus(config *identityv1.AWSWorkloadIdentityConfig, resolved *inventory.Resolution) {
	if config.Spec.Type == identityv1.DeliveryTypeSelfHostedIRSA && resolved.Ready {
		config.Status.ResolvedClusterName = resolved.ClusterName.String()

		return
	}

	config.Status.ResolvedClusterName = ""
}

func setConfigReadyCondition(config *identityv1.AWSWorkloadIdentityConfig, resolved *inventory.Resolution) {
	if !resolved.Ready {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionFalse, resolved.Reason, resolved.Message)

		return
	}

	if propagateNotReady(config, identityv1.ConditionIssuerReady, identityv1.ReasonWaitingForACK, "waiting for issuer resources") {
		return
	}

	if config.Spec.Type == identityv1.DeliveryTypeSelfHostedIRSA {
		if propagateNotReady(config, identityv1.ConditionWebhookRuntimeReady, identityv1.ReasonWebhookRuntimeUnavailable, "waiting for self-hosted webhook runtime") {
			return
		}
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReconciled, "config reconciliation completed")
}

// propagateNotReady mirrors the dependency condition onto Ready=False when the
// dependency is missing or not True, returning true if it short-circuited.
func propagateNotReady(config *identityv1.AWSWorkloadIdentityConfig, depCondType, defaultReason, defaultMessage string) bool {
	cond := meta.FindStatusCondition(config.Status.Conditions, depCondType)
	if cond != nil && cond.Status == metav1.ConditionTrue {
		return false
	}

	reason := defaultReason
	message := defaultMessage

	if cond != nil {
		reason = cond.Reason
		message = cond.Message
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionFalse, reason, message)

	return true
}

func resetSelfHostedStatus(config *identityv1.AWSWorkloadIdentityConfig) {
	config.Status.BucketName = ""
	config.Status.IssuerHostPath = ""
	config.Status.OIDCProviderARN = ""
	config.Status.PublishedKeyID = ""
	config.Status.ResolvedClusterName = ""
	config.Status.WebhookRuntimeNamespace = ""
	config.Status.WebhookRuntimeCertNotAfter = nil
}

func (r *AWSWorkloadIdentityConfigReconciler) reconcileSelfHostedWebhookRuntime(ctx context.Context, log logr.Logger, config *identityv1.AWSWorkloadIdentityConfig, operatorConfig *identityv1.AWSWorkloadIdentityOperatorConfig, resolved *inventory.Resolution) (ctrl.Result, error) {
	if !resolved.Ready {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, resolved.Reason, resolved.Message)

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	target, err := remoteClusterClient(ctx, r.MCManager, resolved)
	if err != nil {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonRemoteClusterUnavailable, err.Error())

		if errors.Is(err, multicluster.ErrClusterNotFound) {
			return ctrl.Result{RequeueAfter: transientRequeue}, nil
		}

		return ctrl.Result{RequeueAfter: transientRequeue}, err
	}

	outcome, err := applyRemoteWebhookRuntime(
		ctx,
		log,
		target,
		webhookRuntimeOwnerForConfig(config),
		operatorConfig.Spec.SelfHostedIRSA.WebhookNamespace,
		r.PodIdentityWebhookImage,
	)
	setWebhookRuntimeObservation(config, outcome.Observation)

	if err != nil {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonWebhookRuntimeApplyFailed, err.Error())

		return ctrl.Result{RequeueAfter: webhookRuntimeRequeueAfter(outcome.Schedule, false)}, err
	}

	if !outcome.Condition.Ready {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, outcome.Condition.Reason, outcome.Condition.Message)

		return ctrl.Result{RequeueAfter: webhookRuntimeRequeueAfter(outcome.Schedule, false)}, nil
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, outcome.Condition.Reason, outcome.Condition.Message)

	return ctrl.Result{RequeueAfter: webhookRuntimeRequeueAfter(outcome.Schedule, true)}, nil
}

func webhookRuntimeRequeueAfter(schedule webhookRuntimeSchedule, ready bool) time.Duration {
	if ready {
		return cmp.Or(schedule.CertRenewalAfter, schedule.ReadinessRetryAfter, transientRequeue)
	}

	return minNonZeroDuration(schedule.ReadinessRetryAfter, schedule.CertRenewalAfter, transientRequeue)
}

func minNonZeroDuration(durations ...time.Duration) time.Duration {
	var smallest time.Duration

	for _, duration := range durations {
		if duration <= 0 {
			continue
		}

		if smallest == 0 || duration < smallest {
			smallest = duration
		}
	}

	return smallest
}

func setWebhookRuntimeObservation(config *identityv1.AWSWorkloadIdentityConfig, observation webhookRuntimeObservation) {
	config.Status.WebhookRuntimeNamespace = observation.WebhookNamespace

	if observation.CertNotAfter.IsZero() {
		config.Status.WebhookRuntimeCertNotAfter = nil

		return
	}

	config.Status.WebhookRuntimeCertNotAfter = ptr.To(observation.CertNotAfter)
}

func setSelfHostedIssuerFailureCondition(config *identityv1.AWSWorkloadIdentityConfig, err error) {
	issuerCondition := meta.FindStatusCondition(config.Status.Conditions, identityv1.ConditionIssuerReady)
	if issuerCondition != nil &&
		issuerCondition.ObservedGeneration == config.Generation &&
		issuerCondition.Status == metav1.ConditionFalse &&
		issuerCondition.Reason == identityv1.ReasonOIDCObjectsPublishFailed {
		return
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonIssuerReconcileFailed, err.Error())
}

func (r *AWSWorkloadIdentityConfigReconciler) reconcileSelfHostedIssuer(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) error {
	log := logf.FromContext(ctx)
	bucketName := identityaws.BucketName(config.Namespace, config.Spec.Region)
	config.Status.BucketName = bucketName
	config.Status.IssuerHostPath = fmt.Sprintf("%s.s3.%s.amazonaws.com", bucketName, config.Spec.Region)

	publicKeyPEM, keyID, err := r.reconcileSigningSecret(ctx, config)
	if err != nil {
		return fmt.Errorf("reconcile signing Secret: %w", err)
	}

	bucketSynced, bucketStatus, err := r.reconcileSelfHostedBucket(ctx, log, config)
	if err != nil {
		return err
	}

	config.Status.ACKResources = []identityv1.ACKResourceStatus{bucketStatus}

	if bucketSynced {
		if config.Status.PublishedKeyID != keyID {
			if err := r.publishSelfHostedOIDCObjects(ctx, config, publicKeyPEM, keyID); err != nil {
				setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionOIDCObjectsPublished, metav1.ConditionFalse, identityv1.ReasonOIDCObjectsPublishFailed, err.Error())
				setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonOIDCObjectsPublishFailed, err.Error())

				return err
			}

			config.Status.PublishedKeyID = keyID
		}

		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionOIDCObjectsPublished, metav1.ConditionTrue, identityv1.ReasonOIDCObjectsPublished, "self-hosted OIDC discovery and JWKS objects are published")
	} else {
		// Bucket not synced: ACK may be (re-)creating it, so previously published
		// objects are no longer guaranteed to exist. Force a re-publish on the
		// next sync by clearing the recorded key ID.
		config.Status.PublishedKeyID = ""
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionOIDCObjectsPublished, metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for ACK S3 Bucket sync before publishing OIDC objects")
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIAMProviderReady, metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for OIDC discovery objects before reconciling IAM OpenIDConnectProvider")
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for ACK resources")

		return nil
	}

	providerSynced, providerStatus, err := r.reconcileOIDCProvider(ctx, log, config)
	if err != nil {
		return err
	}

	config.Status.ACKResources = append(config.Status.ACKResources, providerStatus)

	if providerSynced && config.Status.OIDCProviderARN != "" {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady, "self-hosted issuer resources are synced")
	} else {
		setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting for ACK resources")
	}

	return nil
}

func (r *AWSWorkloadIdentityConfigReconciler) reconcileSelfHostedBucket(ctx context.Context, log logr.Logger, config *identityv1.AWSWorkloadIdentityConfig) (bool, identityv1.ACKResourceStatus, error) {
	bucket, err := identityaws.BuildBucket(config)
	if err != nil {
		return false, identityv1.ACKResourceStatus{}, fmt.Errorf("build self-hosted issuer bucket: %w", err)
	}

	current := &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: bucket.Name, Namespace: bucket.Namespace}}
	op, err := createOrUpdate(ctx, r.Client, r.Scheme, config, current, func() error {
		current.Labels = bucket.Labels
		current.Spec = bucket.Spec

		return nil
	})
	logChildApply(log, metrics.ControllerConfig, ackChildKindBucket, current.Name, op, err)

	if err != nil {
		return false, identityv1.ACKResourceStatus{}, err
	}

	status := identityaws.ACKResourceStatus(s3v1alpha1.GroupVersion.String(), ackChildKindBucket, current, current.Status.Conditions)
	synced := identityaws.IsACKSynced(current.Status.Conditions)
	setACKReadyCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionBucketReady, "S3 Bucket", synced)

	return synced, status, nil
}

func (r *AWSWorkloadIdentityConfigReconciler) reconcileOIDCProvider(ctx context.Context, log logr.Logger, config *identityv1.AWSWorkloadIdentityConfig) (bool, identityv1.ACKResourceStatus, error) {
	desired := identityaws.BuildOIDCProvider(config)
	current := &iamv1alpha1.OpenIDConnectProvider{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	op, err := createOrUpdate(ctx, r.Client, r.Scheme, config, current, func() error {
		current.Labels = desired.Labels
		current.Spec.URL = desired.Spec.URL
		current.Spec.ClientIDs = desired.Spec.ClientIDs
		current.Spec.Tags = desired.Spec.Tags

		return nil
	})
	logChildApply(log, metrics.ControllerConfig, ackChildKindOpenIDConnectProvider, current.Name, op, err)

	if err != nil {
		return false, identityv1.ACKResourceStatus{}, err
	}

	status := identityaws.ACKResourceStatus(iamv1alpha1.GroupVersion.String(), ackChildKindOpenIDConnectProvider, current, current.Status.Conditions)
	config.Status.OIDCProviderARN = identityaws.ARN(current.Status.ACKResourceMetadata)
	synced := identityaws.IsACKSynced(current.Status.Conditions) && config.Status.OIDCProviderARN != ""
	setACKReadyCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIAMProviderReady, "IAM OpenIDConnectProvider", synced)

	return synced, status, nil
}

// selfHostedPublisher returns the S3 publisher for the config's region.
func (r *AWSWorkloadIdentityConfigReconciler) selfHostedPublisher(ctx context.Context, region string) (SelfHostedIssuerPublisher, error) {
	if r.SelfHostedIssuerPublisherFactory == nil {
		return nil, fmt.Errorf("self-hosted issuer publisher is not configured")
	}

	publisher, err := r.SelfHostedIssuerPublisherFactory(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("create S3 OIDC issuer publisher: %w", err)
	}

	return publisher, nil
}

func (r *AWSWorkloadIdentityConfigReconciler) publishSelfHostedOIDCObjects(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig, publicKeyPEM []byte, keyID string) error {
	publisher, err := r.selfHostedPublisher(ctx, config.Spec.Region)
	if err != nil {
		return err
	}

	if err := publisher.PublishOIDCIssuer(ctx, config.Status.BucketName, config.Status.IssuerURL(), publicKeyPEM, keyID); err != nil {
		return fmt.Errorf("publish self-hosted OIDC issuer objects: %w", err)
	}

	return nil
}

func (r *AWSWorkloadIdentityConfigReconciler) deleteSelfHostedOIDCObjects(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig, bucketName string) error {
	publisher, err := r.selfHostedPublisher(ctx, config.Spec.Region)
	if err != nil {
		return err
	}

	if err := publisher.DeleteOIDCIssuer(ctx, bucketName); err != nil {
		return fmt.Errorf("delete self-hosted OIDC issuer objects: %w", err)
	}

	return nil
}

func (r *AWSWorkloadIdentityConfigReconciler) reconcileSigningSecret(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) ([]byte, string, error) {
	log := logf.FromContext(ctx)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: identityaws.SigningKeySecretName(config), Namespace: config.Namespace}}

	op, err := createOrUpdate(ctx, r.Client, r.Scheme, config, secret, func() error {
		setSigningSecretLabels(secret, config)
		ensureAnnotations(secret)

		return ensureSigningSecretData(secret)
	})
	logChildApply(log, metrics.ControllerConfig, "Secret", secret.Name, op, err)

	if err != nil {
		return nil, "", fmt.Errorf("reconcile signing Secret: %w", err)
	}

	publicKey := secret.Data[identityaws.SigningKeyPublicKey]

	keyID := secret.Annotations[identityv1.AnnotationSigningKeyID]
	if len(publicKey) == 0 {
		return nil, "", fmt.Errorf("signing Secret %s/%s is missing %s", secret.Namespace, secret.Name, identityaws.SigningKeyPublicKey)
	}

	if keyID == "" {
		return nil, "", fmt.Errorf("signing Secret %s/%s is missing %s annotation", secret.Namespace, secret.Name, identityv1.AnnotationSigningKeyID)
	}

	return publicKey, keyID, nil
}

func setSigningSecretLabels(secret *corev1.Secret, config *identityv1.AWSWorkloadIdentityConfig) {
	mergeLabels(secret, identityaws.LabelsForConfig(config))
}

func ensureSigningSecretData(secret *corev1.Secret) error {
	if len(secret.Data[identityaws.SigningKeyPrivateKey]) > 0 && len(secret.Data[identityaws.SigningKeyPublicKey]) > 0 {
		return ensureSigningKeyID(secret)
	}

	privateKey, publicKey, keyID, err := oidc.GenerateRSAKeyPEM(2048)
	if err != nil {
		return fmt.Errorf("generate signing key: %w", err)
	}

	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{
		identityaws.SigningKeyPrivateKey: privateKey,
		identityaws.SigningKeyPublicKey:  publicKey,
	}
	secret.Annotations[identityv1.AnnotationSigningKeyID] = keyID

	return nil
}

// ensureSigningKeyID writes the deterministic key ID derived from the current
// public key into the Secret's annotations. It also repairs stale 16-byte key
// IDs left by operator versions before oidc/documents.go switched to the full
// SHA-256 digest.
func ensureSigningKeyID(secret *corev1.Secret) error {
	keyID, err := oidc.KeyIDFromPublicKeyPEM(secret.Data[identityaws.SigningKeyPublicKey])
	if err != nil {
		return fmt.Errorf("derive signing key ID: %w", err)
	}

	annotations := ensureAnnotations(secret)
	if annotations[identityv1.AnnotationSigningKeyID] == keyID {
		return nil
	}

	annotations[identityv1.AnnotationSigningKeyID] = keyID

	return nil
}

func (r *AWSWorkloadIdentityConfigReconciler) reconcileDelete(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := r.List(ctx, roles, client.InNamespace(config.Namespace), client.Limit(1)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list AWSServiceAccountRoles in namespace %q: %w", config.Namespace, err)
	}

	if len(roles.Items) > 0 && !config.IsForceDelete() {
		if err := r.markDeletionBlocked(ctx, log, config, identityv1.ReasonRolesRemain, "AWSServiceAccountRole objects remain in namespace"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	if err := r.deleteRemoteWebhookRuntime(ctx, config); err != nil {
		if patchErr := r.markDeletionBlocked(ctx, log, config, identityv1.ReasonRemoteClusterUnavailable, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}

		return ctrl.Result{RequeueAfter: transientRequeue}, nil
	}

	if err := r.deleteSelfHostedChildren(ctx, config); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete self-hosted children: %w", err)
	}

	if err := removeFinalizer(ctx, r.Client, config, identityv1.ConfigFinalizer); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("finalizer removed", "awio.operation", "remove_finalizer")
	recordFinalizerRemoved(r.Recorder, config)

	return ctrl.Result{}, nil
}

func (r *AWSWorkloadIdentityConfigReconciler) deleteRemoteWebhookRuntime(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) error {
	if config.Spec.Type != identityv1.DeliveryTypeSelfHostedIRSA {
		return nil
	}

	if err := r.deleteRemoteWebhookRuntimeStrict(ctx, config); err != nil {
		if config.IsForceDelete() {
			return nil
		}

		return err
	}

	return nil
}

func (r *AWSWorkloadIdentityConfigReconciler) deleteRemoteWebhookRuntimeStrict(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) error {
	resolved, err := r.Resolver.Resolve(ctx, config.Namespace)
	if err != nil {
		return fmt.Errorf("resolve inventory for remote webhook runtime cleanup: %w", err)
	}

	if !resolved.Ready {
		return fmt.Errorf("%s: %s", resolved.Reason, resolved.Message)
	}

	shared, err := r.remoteRuntimeSharedByOtherConfig(ctx, config, &resolved)
	if err != nil {
		return err
	}

	if shared {
		return nil
	}

	target, err := remoteClusterClient(ctx, r.MCManager, &resolved)
	if err != nil {
		return fmt.Errorf("resolve remote cluster client for webhook runtime cleanup: %w", err)
	}

	namespace, err := r.webhookRuntimeNamespaceForDeletion(ctx, config)
	if err != nil {
		return err
	}

	return deleteRemoteWebhookRuntime(ctx, target, namespace)
}

func (r *AWSWorkloadIdentityConfigReconciler) remoteRuntimeSharedByOtherConfig(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig, resolved *inventory.Resolution) (bool, error) {
	indexedConfigs := &identityv1.AWSWorkloadIdentityConfigList{}
	if err := r.List(ctx, indexedConfigs, configByResolvedClusterKey(resolved.ClusterName.String())); err != nil {
		return false, fmt.Errorf("list AWSWorkloadIdentityConfigs by resolved cluster %q for remote runtime cleanup: %w", resolved.ClusterName.String(), err)
	}

	for i := range indexedConfigs.Items {
		other := &indexedConfigs.Items[i]
		if isLiveSelfHostedSiblingConfig(config, other) {
			return true, nil
		}
	}

	configs := &identityv1.AWSWorkloadIdentityConfigList{}
	if err := r.List(ctx, configs); err != nil {
		return false, fmt.Errorf("list AWSWorkloadIdentityConfigs for conservative remote runtime cleanup check: %w", err)
	}

	for i := range configs.Items {
		other := &configs.Items[i]
		if !isLiveSelfHostedSiblingConfig(config, other) {
			continue
		}

		if other.Status.ResolvedClusterName == resolved.ClusterName.String() {
			return true, nil
		}

		// ResolvedClusterName is only a positive fast path. A different cached
		// value cannot prove the sibling is still on another cluster because
		// inventory can remap without changing the config generation.
		otherResolved, err := r.Resolver.Resolve(ctx, other.Namespace)
		if err != nil {
			return false, fmt.Errorf("resolve sibling AWSWorkloadIdentityConfig %s/%s during remote runtime cleanup: %w", other.Namespace, other.Name, err)
		}

		if !otherResolved.Ready {
			return false, fmt.Errorf("sibling AWSWorkloadIdentityConfig %s/%s inventory is not ready during remote runtime cleanup: %s: %s", other.Namespace, other.Name, otherResolved.Reason, otherResolved.Message)
		}

		if otherResolved.ClusterName == resolved.ClusterName {
			return true, nil
		}
	}

	return false, nil
}

func isLiveSelfHostedSiblingConfig(config, other *identityv1.AWSWorkloadIdentityConfig) bool {
	sameObject := other.Namespace == config.Namespace && other.Name == config.Name
	if config.UID != "" {
		sameObject = other.UID == config.UID
	}

	return !sameObject &&
		other.Spec.Type == identityv1.DeliveryTypeSelfHostedIRSA &&
		other.DeletionTimestamp.IsZero()
}

func (r *AWSWorkloadIdentityConfigReconciler) webhookRuntimeNamespaceForDeletion(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) (string, error) {
	if config.Status.WebhookRuntimeNamespace != "" {
		return config.Status.WebhookRuntimeNamespace, nil
	}

	// Status.WebhookRuntimeNamespace is the canonical record of where the
	// runtime was applied; if it was never persisted (e.g. crash between apply
	// and status patch) the OperatorConfig is the only authoritative source of
	// the configured namespace. Force-delete bypasses this at the caller.
	operatorConfig, err := loadOperatorConfig(ctx, r.Client)
	if err != nil {
		return "", fmt.Errorf("resolve webhook namespace for cleanup: %w", err)
	}

	return operatorConfig.Spec.SelfHostedIRSA.WebhookNamespace, nil
}

// deleteSelfHostedChildren cleans up the hub-cluster ACK CRs and S3 issuer
// objects. The OIDC provider, signing Secret, and (S3 objects → bucket) chain
// are independent of each other, so they run in parallel.
func (r *AWSWorkloadIdentityConfigReconciler) deleteSelfHostedChildren(ctx context.Context, config *identityv1.AWSWorkloadIdentityConfig) error {
	bucketName := config.Status.BucketName
	if bucketName == "" {
		bucketName = identityaws.BucketName(config.Namespace, config.Spec.Region)
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		obj := &iamv1alpha1.OpenIDConnectProvider{ObjectMeta: metav1.ObjectMeta{Name: identityaws.OIDCProviderName(config), Namespace: config.Namespace}}
		if err := client.IgnoreNotFound(r.Delete(gCtx, obj)); err != nil {
			return fmt.Errorf("delete OpenIDConnectProvider %s/%s: %w", obj.Namespace, obj.Name, err)
		}

		return nil
	})

	g.Go(func() error {
		obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: identityaws.SigningKeySecretName(config), Namespace: config.Namespace}}
		if err := client.IgnoreNotFound(r.Delete(gCtx, obj)); err != nil {
			return fmt.Errorf("delete signing Secret %s/%s: %w", obj.Namespace, obj.Name, err)
		}

		return nil
	})

	g.Go(func() error {
		if config.Spec.Type == identityv1.DeliveryTypeSelfHostedIRSA {
			if err := r.deleteSelfHostedOIDCObjects(gCtx, config, bucketName); err != nil {
				return fmt.Errorf("delete OIDC issuer objects: %w", err)
			}
		}

		bucket := &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: config.Namespace}}
		if err := client.IgnoreNotFound(r.Delete(gCtx, bucket)); err != nil {
			return fmt.Errorf("delete Bucket %s/%s: %w", bucket.Namespace, bucket.Name, err)
		}

		return nil
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("delete self-hosted children: %w", err)
	}

	return nil
}

func (r *AWSWorkloadIdentityConfigReconciler) markDeletionBlocked(ctx context.Context, log logr.Logger, config *identityv1.AWSWorkloadIdentityConfig, reason, message string) error {
	beforeStatus := config.Status.DeepCopy()
	config.Status.ObservedGeneration = config.Generation
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionDeletionBlocked, metav1.ConditionTrue, reason, message)
	log.Info("deletion blocked", "awio.condition.type", identityv1.ConditionDeletionBlocked, "awio.condition.reason", reason)

	return r.patchConfigStatus(ctx, log, config, beforeStatus)
}

func (r *AWSWorkloadIdentityConfigReconciler) patchConfigStatus(ctx context.Context, log logr.Logger, config *identityv1.AWSWorkloadIdentityConfig, beforeStatus *identityv1.AWSWorkloadIdentityConfigStatus) error {
	if apiequality.Semantic.DeepEqual(*beforeStatus, config.Status) {
		return nil
	}

	patchBase := config.DeepCopy()
	patchBase.Status = *beforeStatus

	return patchStatusAndObserve(ctx, log, r.Status(), r.Recorder, metrics.ControllerConfig, config, patchBase, beforeStatus.Conditions, config.Status.Conditions)
}

// SetupWithManager registers the reconciler with a controller manager.
func (r *AWSWorkloadIdentityConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Resolver.Client == nil {
		r.Resolver = inventory.Resolver{Client: r.Client}
	}

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&identityv1.AWSWorkloadIdentityConfig{}).
		Owns(&corev1.Secret{}).
		Owns(&iamv1alpha1.OpenIDConnectProvider{}).
		Owns(&s3v1alpha1.Bucket{}).
		Watches(&identityv1.AWSWorkloadIdentityOperatorConfig{}, handler.EnqueueRequestsFromMapFunc(r.configsForOperatorConfig)).
		Watches(&clusterinventoryv1alpha1.ClusterProfile{}, handler.EnqueueRequestsFromMapFunc(configForClusterProfile)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles})

	if r.RuntimeEventChannel != nil {
		builder = builder.WatchesRawSource(source.Channel[*identityv1.AWSWorkloadIdentityConfig](
			r.RuntimeEventChannel,
			&handler.TypedEnqueueRequestForObject[*identityv1.AWSWorkloadIdentityConfig]{},
		))
	}

	if err := builder.Complete(r); err != nil {
		return fmt.Errorf("set up AWSWorkloadIdentityConfig controller: %w", err)
	}

	return nil
}

func (r *AWSWorkloadIdentityConfigReconciler) configsForOperatorConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	if !isDefaultObject(obj) {
		return nil
	}

	return requestsForList(ctx, r.Client, &identityv1.AWSWorkloadIdentityConfigList{})
}

func configForClusterProfile(_ context.Context, obj client.Object) []reconcile.Request {
	profile, ok := obj.(*clusterinventoryv1alpha1.ClusterProfile)
	if !ok {
		return nil
	}

	namespace := inventory.WorkloadNamespaceForClusterProfile(profile)
	if namespace == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: namespace,
			Name:      identityv1.DefaultName,
		},
	}}
}
