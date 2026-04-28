package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/record"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
)

const (
	testConfigUID               = "config-uid"
	testInventoryNamespace      = "wlc-a"
	testIssuerBucketNameValue   = "issuer-bucket"
	testPodIdentityWebhookImage = "example.com/pod-identity-webhook:test"
	testPreservedValue          = "keep"
	testResolvedClusterName     = testInventoryNamespace + "/" + testInventoryNamespace
	testRoleARN                 = "arn:aws:iam::123456789012:role/app"
	testRoleUID                 = "role-uid"
	testSiblingConfigUID        = "sibling-uid"
	testSiblingNamespace        = "wlc-b"
	testWebhookNamespace        = "custom-webhook"
)

func TestReconcileNormalSelfHostedIssuerFailureDoesNotSkipRuntimeStatusOrReturnIgnoredRuntimeRetry(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{publishErr: errors.New("s3 access denied")}
	remoteClient := fakeClient(t, availableWebhookDeployment())
	localClient := testSelfHostedComponentsClient(t,
		config,
		testOperatorConfig(),
		testResolvedClusterProfile(config.Namespace),
		testBucket(config, true),
	)
	reconciler := testSelfHostedComponentsReconciler(t, localClient, publisher, remoteClient, nil)

	result, err := reconciler.reconcileNormal(ctx, config)
	if err == nil || !strings.Contains(err.Error(), "s3 access denied") {
		t.Fatalf("expected issuer publish error, got result=%#v err=%v", result, err)
	}

	if !result.IsZero() {
		t.Fatalf("expected empty result when issuer error bubbles, got %#v", result)
	}

	stored := getStoredConfig(t, localClient, config)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonOIDCObjectsPublishFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, identityv1.ReasonWebhookRuntimeSynced)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonOIDCObjectsPublishFailed)

	if stored.Status.WebhookRuntimeNamespace != testWebhookNamespace {
		t.Fatalf("expected runtime namespace to merge despite issuer failure, got %q", stored.Status.WebhookRuntimeNamespace)
	}

	if stored.Status.WebhookRuntimeCertNotAfter == nil {
		t.Fatal("expected runtime certificate observation to merge despite issuer failure")
	}

	if stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name to be recorded, got %q", stored.Status.ResolvedClusterName)
	}
}

func TestReconcileNormalSelfHostedSuccessMergesIssuerAndRuntimeStatus(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{}
	remoteClient := fakeClient(t, availableWebhookDeployment())
	localClient := testSelfHostedComponentsClient(t,
		config,
		testOperatorConfig(),
		testResolvedClusterProfile(config.Namespace),
		testBucket(config, true),
		testOIDCProvider(config, "arn:aws:iam::123456789012:oidc-provider/example"),
	)
	reconciler := testSelfHostedComponentsReconciler(t, localClient, publisher, remoteClient, nil)

	result, err := reconciler.reconcileNormal(ctx, config)
	if err != nil {
		t.Fatalf("expected successful reconcile, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != dependencySteadyStateRequeue {
		t.Fatalf("expected dependency safety requeue %s to cap runtime certificate requeue, got %#v", dependencySteadyStateRequeue, result)
	}

	stored := getStoredConfig(t, localClient, config)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, identityv1.ReasonWebhookRuntimeSynced)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReconciled)

	if len(stored.Status.ACKResources) != 2 {
		t.Fatalf("expected issuer ACKResources for Bucket and OpenIDConnectProvider, got %#v", stored.Status.ACKResources)
	}

	if stored.Status.BucketName != testIssuerBucketName(config) {
		t.Fatalf("expected issuer bucket status to merge, got %q", stored.Status.BucketName)
	}

	if stored.Status.PublishedKeyID == "" {
		t.Fatal("expected issuer published key ID to merge")
	}

	if stored.Status.OIDCProviderARN == "" {
		t.Fatal("expected issuer OIDC provider ARN to merge")
	}

	if stored.Status.WebhookRuntimeNamespace != testWebhookNamespace {
		t.Fatalf("expected runtime namespace to merge, got %q", stored.Status.WebhookRuntimeNamespace)
	}

	if stored.Status.WebhookRuntimeCertNotAfter == nil {
		t.Fatal("expected runtime certificate observation to merge")
	}

	if stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name to be recorded, got %q", stored.Status.ResolvedClusterName)
	}
}

func TestReconcileNormalSelfHostedRuntimeNotReadyUsesReadinessRetry(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{}
	remoteClient := fakeClient(t)
	localClient := testSelfHostedComponentsClient(t,
		config,
		testOperatorConfig(),
		testResolvedClusterProfile(config.Namespace),
		testBucket(config, true),
		testOIDCProvider(config, "arn:aws:iam::123456789012:oidc-provider/example"),
	)
	reconciler := testSelfHostedComponentsReconciler(t, localClient, publisher, remoteClient, nil)

	result, err := reconciler.reconcileNormal(ctx, config)
	if err != nil {
		t.Fatalf("expected successful not-ready reconcile, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected not-ready runtime to use readiness retry %s, got %s", transientRequeue, result.RequeueAfter)
	}

	stored := getStoredConfig(t, localClient, config)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonWebhookDeploymentRolloutInProgress)

	if stored.Status.WebhookRuntimeCertNotAfter == nil {
		t.Fatal("expected certificate observation to be recorded while runtime waits for Deployment availability")
	}

	if stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name to be recorded, got %q", stored.Status.ResolvedClusterName)
	}
}

// TestReconcileNormalSelfHostedRuntimeFailurePersistsStatusAndBubblesError pins
// the contract that a non-NotFound runtime infrastructure error bubbles up from
// reconcileNormal so the workqueue applies exponential rate-limiting, while the
// status patch (issuer success + runtime failure conditions) still completes
// before the error return so observers see a consistent merged status.
func TestReconcileNormalSelfHostedRuntimeFailurePersistsStatusAndBubblesError(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{}
	remoteErr := errors.New("remote cluster unavailable")
	localClient := testSelfHostedComponentsClient(t,
		config,
		testOperatorConfig(),
		testResolvedClusterProfile(config.Namespace),
		testBucket(config, true),
		testOIDCProvider(config, "arn:aws:iam::123456789012:oidc-provider/example"),
	)
	reconciler := testSelfHostedComponentsReconciler(t, localClient, publisher, nil, remoteErr)

	result, err := reconciler.reconcileNormal(ctx, config)
	if err == nil {
		t.Fatalf("expected non-NotFound runtime error to bubble, got result=%#v err=nil", result)
	}

	if !strings.Contains(err.Error(), "resolve remote cluster for self-hosted webhook runtime") {
		t.Fatalf("expected error to be wrapped with the runtime-resolve prefix, got %v", err)
	}

	if !strings.Contains(err.Error(), remoteErr.Error()) {
		t.Fatalf("expected error to wrap the original remote error %q, got %v", remoteErr.Error(), err)
	}

	if !result.IsZero() {
		t.Fatalf("expected empty result when runtime error bubbles, got %#v", result)
	}

	stored := getStoredConfig(t, localClient, config)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonRemoteClusterUnavailable)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonRemoteClusterUnavailable)

	if stored.Status.BucketName != testIssuerBucketName(config) {
		t.Fatalf("expected issuer bucket status to merge, got %q", stored.Status.BucketName)
	}

	if len(stored.Status.ACKResources) != 2 {
		t.Fatalf("expected issuer ACKResources to merge despite runtime failure, got %#v", stored.Status.ACKResources)
	}

	if stored.Status.PublishedKeyID == "" {
		t.Fatal("expected issuer published key ID to merge despite runtime failure")
	}

	if stored.Status.OIDCProviderARN == "" {
		t.Fatal("expected issuer OIDC provider ARN to merge despite runtime failure")
	}

	if stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name to be recorded despite runtime failure, got %q", stored.Status.ResolvedClusterName)
	}
}

// TestReconcileNormalSelfHostedReportsBothIssuerAndRuntimeErrors pins the
// contract that when both the issuer and the webhook-runtime paths fail, both
// errors are joined and bubbled together, while the merged conditions for both
// failures are persisted before the error return.
func TestReconcileNormalSelfHostedReportsBothIssuerAndRuntimeErrors(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	issuerErr := errors.New("issuer publish denied")
	runtimeErr := errors.New("remote client unavailable")
	publisher := &fakeOIDCIssuerPublisher{publishErr: issuerErr}
	localClient := testSelfHostedComponentsClient(t,
		config,
		testOperatorConfig(),
		testResolvedClusterProfile(config.Namespace),
		testBucket(config, true),
	)
	reconciler := testSelfHostedComponentsReconciler(t, localClient, publisher, nil, runtimeErr)

	result, err := reconciler.reconcileNormal(ctx, config)
	if err == nil {
		t.Fatalf("expected joined issuer + runtime error, got result=%#v err=nil", result)
	}

	if !strings.Contains(err.Error(), issuerErr.Error()) {
		t.Fatalf("expected joined error to include issuer failure %q, got %v", issuerErr.Error(), err)
	}

	if !strings.Contains(err.Error(), runtimeErr.Error()) {
		t.Fatalf("expected joined error to include runtime failure %q, got %v", runtimeErr.Error(), err)
	}

	if !strings.Contains(err.Error(), "resolve remote cluster for self-hosted webhook runtime") {
		t.Fatalf("expected joined error to include the runtime-resolve wrap prefix, got %v", err)
	}

	if !result.IsZero() {
		t.Fatalf("expected empty result when joined error bubbles, got %#v", result)
	}

	stored := getStoredConfig(t, localClient, config)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonOIDCObjectsPublishFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonRemoteClusterUnavailable)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonOIDCObjectsPublishFailed)
}

// TestReconcileSelfHostedWebhookRuntimeBubblesNonNotFoundClusterError pins the
// branch in reconcileSelfHostedWebhookRuntime where a remote-cluster resolution
// failure is non-NotFound: the error must bubble (so the workqueue
// exponential-rate-limits) wrapped with the resolve-remote-cluster prefix, and
// the level-triggered status condition must still be set so observers see the
// failure regardless of which path the caller patches via.
func TestReconcileSelfHostedWebhookRuntimeBubblesNonNotFoundClusterError(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	operatorConfig := testOperatorConfig()
	resolved := readyResolution()
	remoteErr := errors.New("dial tcp: connection refused")
	reconciler := testSelfHostedComponentsReconciler(t, fakeClient(t), &fakeOIDCIssuerPublisher{}, nil, remoteErr)

	result, err := reconciler.reconcileSelfHostedWebhookRuntime(ctx, logr.Discard(), config, operatorConfig, resolved)
	if err == nil {
		t.Fatalf("expected non-NotFound remote cluster error to bubble, got result=%#v err=nil", result)
	}

	if !strings.Contains(err.Error(), "resolve remote cluster for self-hosted webhook runtime") {
		t.Fatalf("expected error to be wrapped with the runtime-resolve prefix, got %v", err)
	}

	if !errors.Is(err, remoteErr) {
		t.Fatalf("expected wrapped error to satisfy errors.Is on the original remoteErr, got %v", err)
	}

	assertCondition(t, config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonRemoteClusterUnavailable)
}

// TestReconcileSelfHostedWebhookRuntimeReturnsNilErrorOnClusterNotFound pins
// the level-triggered branch: when the multicluster manager reports
// ErrClusterNotFound the controller waits for the inventory-manager event
// rather than burning workqueue exponential backoff, so err is nil and the
// requeue uses the fixed transient interval.
func TestReconcileSelfHostedWebhookRuntimeReturnsNilErrorOnClusterNotFound(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	operatorConfig := testOperatorConfig()
	resolved := readyResolution()
	notFoundErr := fmt.Errorf("workload cluster not yet registered: %w", multicluster.ErrClusterNotFound)
	reconciler := testSelfHostedComponentsReconciler(t, fakeClient(t), &fakeOIDCIssuerPublisher{}, nil, notFoundErr)

	result, err := reconciler.reconcileSelfHostedWebhookRuntime(ctx, logr.Discard(), config, operatorConfig, resolved)
	if err != nil {
		t.Fatalf("expected nil error for ErrClusterNotFound (level-triggered), got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue %s on ErrClusterNotFound, got %#v", transientRequeue, result)
	}

	assertCondition(t, config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonRemoteClusterUnavailable)
}

// TestReconcileSelfHostedWebhookRuntimeBubblesApplyError pins the branch where
// applyRemoteWebhookRuntime fails inside reconcileSelfHostedWebhookRuntime. The
// error must bubble wrapped with the apply prefix so the workqueue
// exponential-rate-limits, and the WebhookRuntimeReady condition must record
// the apply failure for the caller's status patch.
func TestReconcileSelfHostedWebhookRuntimeBubblesApplyError(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	operatorConfig := testOperatorConfig()
	resolved := readyResolution()
	createErr := errors.New("simulated namespace create failure")
	remoteClient := fake.NewClientBuilder().
		WithScheme(testSelfHostedComponentsScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return createErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := testSelfHostedComponentsReconciler(t, fakeClient(t), &fakeOIDCIssuerPublisher{}, remoteClient, nil)

	result, err := reconciler.reconcileSelfHostedWebhookRuntime(ctx, logr.Discard(), config, operatorConfig, resolved)
	if err == nil {
		t.Fatalf("expected applyRemoteWebhookRuntime failure to bubble, got result=%#v err=nil", result)
	}

	if !strings.Contains(err.Error(), "apply self-hosted webhook runtime") {
		t.Fatalf("expected error to be wrapped with the apply prefix, got %v", err)
	}

	if !errors.Is(err, createErr) {
		t.Fatalf("expected wrapped error to satisfy errors.Is on the injected create error, got %v", err)
	}

	assertCondition(t, config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonWebhookRuntimeApplyFailed)
}

// TestReconcileSelfHostedWebhookRuntimeReturnsNilErrorWhenInventoryNotReady
// pins the level-triggered branch where the inventory resolution is not yet
// ready: the controller waits for the inventory-resolver event rather than
// burning workqueue exponential backoff, so err is nil, the requeue uses the
// fixed transient interval, and the level-triggered WebhookRuntimeReady=False
// condition mirrors resolved.Reason / resolved.Message so observers can
// distinguish "waiting on inventory" from a real apply failure.
func TestReconcileSelfHostedWebhookRuntimeReturnsNilErrorWhenInventoryNotReady(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	operatorConfig := testOperatorConfig()
	resolved := &inventory.Resolution{
		Ready:   false,
		Reason:  identityv1.ReasonInventoryUnavailable,
		Message: "inventory cluster profile not yet observed",
	}
	reconciler := testSelfHostedComponentsReconciler(t, testSelfHostedComponentsClient(t), &fakeOIDCIssuerPublisher{}, nil, nil)

	result, err := reconciler.reconcileSelfHostedWebhookRuntime(ctx, logr.Discard(), config, operatorConfig, resolved)
	if err != nil {
		t.Fatalf("expected nil error for level-triggered inventory-not-ready branch, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue %s on inventory-not-ready, got %#v", transientRequeue, result)
	}

	assertCondition(t, config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, resolved.Reason)

	cond := meta.FindStatusCondition(config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady)
	if cond.Message != resolved.Message {
		t.Fatalf("expected condition Message to mirror resolved.Message %q, got %q", resolved.Message, cond.Message)
	}
}

// TestReconcileSelfHostedWebhookRuntimeReturnsNilErrorWhenRuntimeNotYetReady
// pins the level-triggered branch where applyRemoteWebhookRuntime succeeds at
// writing all resources but the resulting Deployment is not yet Available.
// The controller intentionally returns a schedule-aware requeue alongside a
// nil error so the cert-renewal / readiness-retry timer governs the next
// observation rather than the workqueue's exponential backoff. The
// WebhookRuntimeReady=False condition records outcome.Condition.Reason /
// Message so observers can distinguish "waiting on Deployment" from a real
// apply failure.
func TestReconcileSelfHostedWebhookRuntimeReturnsNilErrorWhenRuntimeNotYetReady(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testSelfHostedConfig()
	operatorConfig := testOperatorConfig()
	// fakeClient(t) without availableWebhookDeployment seeded mirrors
	// TestApplyRemoteWebhookRuntimeWaitsForDeploymentBeforeCreatingWebhookConfiguration:
	// applyRemoteWebhookRuntime succeeds in writing resources but the resulting
	// Deployment is not Available, so outcome.Condition.Ready is false.
	remoteClient := fakeClient(t)
	resolved := readyResolution()
	reconciler := testSelfHostedComponentsReconciler(t, testSelfHostedComponentsClient(t), &fakeOIDCIssuerPublisher{}, remoteClient, nil)

	result, err := reconciler.reconcileSelfHostedWebhookRuntime(ctx, logr.Discard(), config, operatorConfig, resolved)
	if err != nil {
		t.Fatalf("expected nil error for level-triggered runtime-not-yet-ready branch, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter <= 0 {
		t.Fatalf("expected non-zero RequeueAfter (cert-renewal / readiness retry), got %#v", result)
	}

	assertCondition(t, config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonWebhookDeploymentRolloutInProgress)

	cond := meta.FindStatusCondition(config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady)
	if cond.Message == "" {
		t.Fatal("expected non-empty condition Message on runtime-not-yet-ready")
	}
}

// readyResolution constructs a minimally-valid Ready inventory.Resolution used
// by the direct reconcileSelfHostedWebhookRuntime tests above. The cluster name
// matches the convention `namespace/namespace` used by the resolver and
// exercised by the existing reconcileNormal tests.
func readyResolution() *inventory.Resolution {
	return &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
		Reason:      identityv1.ReasonResolved,
		Message:     "ClusterProfile resolved",
	}
}

func TestSelfHostedStatusMergesOwnedConditionsBeforeReadyComputed(t *testing.T) {
	config := testSelfHostedConfig()
	issuerConfig := config.DeepCopy()
	runtimeConfig := config.DeepCopy()
	certNotAfter := metav1.Now()

	issuerConfig.Status.BucketName = testIssuerBucketNameValue
	issuerConfig.Status.IssuerHostPath = testIssuerBucketNameValue + ".s3.ap-northeast-1.amazonaws.com"
	issuerConfig.Status.OIDCProviderARN = "arn:aws:iam::123456789012:oidc-provider/example"
	issuerConfig.Status.PublishedKeyID = "key-id"
	setCondition(&issuerConfig.Status.Conditions, issuerConfig.Generation, identityv1.ConditionBucketReady, metav1.ConditionTrue, identityv1.ReasonACKResourceSynced, "bucket ready")
	setCondition(&issuerConfig.Status.Conditions, issuerConfig.Generation, identityv1.ConditionOIDCObjectsPublished, metav1.ConditionTrue, identityv1.ReasonOIDCObjectsPublished, "published")
	setCondition(&issuerConfig.Status.Conditions, issuerConfig.Generation, identityv1.ConditionIAMProviderReady, metav1.ConditionTrue, identityv1.ReasonACKResourceSynced, "provider ready")
	setCondition(&issuerConfig.Status.Conditions, issuerConfig.Generation, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady, "issuer ready")
	setCondition(&issuerConfig.Status.Conditions, issuerConfig.Generation, identityv1.ConditionReady, metav1.ConditionFalse, "ChildReadyShouldNotMerge", "child ready")

	runtimeConfig.Status.WebhookRuntimeNamespace = testWebhookNamespace
	runtimeConfig.Status.WebhookRuntimeCertNotAfter = &certNotAfter
	setCondition(&runtimeConfig.Status.Conditions, runtimeConfig.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, identityv1.ReasonWebhookRuntimeSynced, "runtime ready")
	setCondition(&runtimeConfig.Status.Conditions, runtimeConfig.Generation, identityv1.ConditionReady, metav1.ConditionFalse, "RuntimeReadyShouldNotMerge", "child ready")

	mergeSelfHostedIssuerStatus(config, issuerConfig)
	mergeSelfHostedWebhookRuntimeStatus(config, runtimeConfig)

	if meta.FindStatusCondition(config.Status.Conditions, identityv1.ConditionReady) != nil {
		t.Fatalf("expected child Ready conditions not to merge, got %#v", config.Status.Conditions)
	}

	assertCondition(t, config.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady)
	assertCondition(t, config.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, identityv1.ReasonWebhookRuntimeSynced)

	setConfigReadyCondition(config, &inventory.Resolution{
		Ready:   true,
		Reason:  identityv1.ReasonResolved,
		Message: "ClusterProfile resolved",
	})

	assertCondition(t, config.Status.Conditions, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReconciled)
}

func TestCopyConditionByTypeCopiesOnlyRequestedCondition(t *testing.T) {
	var dst []metav1.Condition

	src := []metav1.Condition{
		{
			Type:               identityv1.ConditionIssuerReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 7,
			Reason:             identityv1.ReasonReady,
			Message:            "issuer ready",
		},
		{
			Type:               identityv1.ConditionReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: 7,
			Reason:             "DoNotCopy",
			Message:            "ready should not be copied",
		},
	}

	copyConditionByType(&dst, src, identityv1.ConditionIssuerReady)
	copyConditionByType(&dst, src, identityv1.ConditionWebhookRuntimeReady)

	if len(dst) != 1 {
		t.Fatalf("expected exactly one copied condition, got %#v", dst)
	}

	condition := meta.FindStatusCondition(dst, identityv1.ConditionIssuerReady)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != identityv1.ReasonReady || condition.ObservedGeneration != 7 {
		t.Fatalf("unexpected copied condition: %#v", condition)
	}

	if meta.FindStatusCondition(dst, identityv1.ConditionReady) != nil {
		t.Fatalf("expected Ready not to be copied unless requested, got %#v", dst)
	}
}

func testSelfHostedComponentsReconciler(t *testing.T, localClient client.Client, publisher SelfHostedIssuerPublisher, remoteClient client.Client, remoteErr error) *AWSWorkloadIdentityConfigReconciler {
	t.Helper()

	return &AWSWorkloadIdentityConfigReconciler{
		Client:                  localClient,
		Scheme:                  testSelfHostedComponentsScheme(t),
		Resolver:                inventory.Resolver{Client: localClient},
		MCManager:               &testRemoteClusterGetter{client: remoteClient, err: remoteErr},
		PodIdentityWebhookImage: testPodIdentityWebhookImage,
		SelfHostedIssuerPublisherFactory: func(_ context.Context, region string) (SelfHostedIssuerPublisher, error) {
			if region != "ap-northeast-1" {
				return nil, fmt.Errorf("unexpected publisher region %q", region)
			}

			return publisher, nil
		},
	}
}

func testSelfHostedComponentsClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testSelfHostedComponentsScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		Build()
}

func testSelfHostedComponentsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := testControllerScheme(t)
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return scheme
}

func testOperatorConfig() *identityv1.AWSWorkloadIdentityOperatorConfig {
	return &identityv1.AWSWorkloadIdentityOperatorConfig{
		ObjectMeta: metav1.ObjectMeta{Name: identityv1.DefaultName},
		Spec: identityv1.AWSWorkloadIdentityOperatorConfigSpec{
			SelfHostedIRSA: identityv1.SelfHostedIRSAConfig{
				WebhookNamespace: testWebhookNamespace,
			},
		},
	}
}

func testResolvedClusterProfile(namespace string) *clusterinventoryv1alpha1.ClusterProfile {
	return &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: namespace, Namespace: namespace},
		Status: clusterinventoryv1alpha1.ClusterProfileStatus{
			AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{Name: "ocm"}},
			Properties: []clusterinventoryv1alpha1.Property{
				{Name: inventory.PropertyEKSClusterName, Value: namespace},
				{Name: inventory.PropertyEKSClusterARN, Value: "arn:aws:eks:ap-northeast-1:123456789012:cluster/" + namespace},
				{Name: inventory.PropertyAWSAccountID, Value: "123456789012"},
			},
		},
	}
}

func testOIDCProvider(config *identityv1.AWSWorkloadIdentityConfig, arn string) *iamv1alpha1.OpenIDConnectProvider {
	resourceARN := ackv1alpha1.AWSResourceName(arn)

	return &iamv1alpha1.OpenIDConnectProvider{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.OIDCProviderName(config), Namespace: config.Namespace},
		Status: iamv1alpha1.OpenIDConnectProviderStatus{
			ACKResourceMetadata: &ackv1alpha1.ResourceMetadata{ARN: &resourceARN},
			Conditions: []*ackv1alpha1.Condition{{
				Type:   ackv1alpha1.ConditionTypeResourceSynced,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func getStoredConfig(t *testing.T, c client.Client, config *identityv1.AWSWorkloadIdentityConfig) *identityv1.AWSWorkloadIdentityConfig {
	t.Helper()

	stored := &identityv1.AWSWorkloadIdentityConfig{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(config), stored); err != nil {
		t.Fatal(err)
	}

	return stored
}

func assertCondition(t *testing.T, conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()

	condition := meta.FindStatusCondition(conditions, condType)
	if condition == nil {
		t.Fatalf("expected %s condition, got %#v", condType, conditions)
	}

	if condition.Status != status || condition.Reason != reason {
		t.Fatalf("expected %s=%s/%s, got %s/%s: %#v", condType, status, reason, condition.Status, condition.Reason, condition)
	}
}

type testRemoteClusterGetter struct {
	client client.Client
	err    error
}

func (g *testRemoteClusterGetter) GetCluster(context.Context, multicluster.ClusterName) (cluster.Cluster, error) {
	if g.err != nil {
		return nil, g.err
	}

	return &testClientCluster{client: g.client}, nil
}

type testClientCluster struct {
	client client.Client
}

func (c *testClientCluster) GetHTTPClient() *http.Client {
	return nil
}

func (c *testClientCluster) GetConfig() *rest.Config {
	return nil
}

func (c *testClientCluster) GetCache() cache.Cache {
	return nil
}

func (c *testClientCluster) GetScheme() *runtime.Scheme {
	return runtime.NewScheme()
}

func (c *testClientCluster) GetClient() client.Client {
	return c.client
}

func (c *testClientCluster) GetFieldIndexer() client.FieldIndexer {
	return nil
}

func (c *testClientCluster) GetRESTMapper() meta.RESTMapper {
	return meta.NewDefaultRESTMapper([]schema.GroupVersion{})
}

func (c *testClientCluster) GetAPIReader() client.Reader {
	return c.client
}

func (c *testClientCluster) Start(context.Context) error {
	return nil
}

func (c *testClientCluster) GetEventRecorderFor(string) record.EventRecorder {
	return nil
}

func (c *testClientCluster) GetEventRecorder(string) events.EventRecorder {
	return nil
}

var _ cluster.Cluster = (*testClientCluster)(nil)
var _ remoteClusterGetter = (*testRemoteClusterGetter)(nil)
