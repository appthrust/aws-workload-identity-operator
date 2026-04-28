package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	eksv1alpha1 "github.com/aws-controllers-k8s/eks-controller/apis/v1alpha1"
	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	s3v1alpha1 "github.com/aws-controllers-k8s/s3-controller/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
)

type fakeOIDCIssuerPublisher struct {
	bucket       string
	deleteCalls  int
	deleteErr    error
	issuerURL    string
	publicKeyPEM []byte
	keyID        string
	publishErr   error
	calls        int
}

func (f *fakeOIDCIssuerPublisher) DeleteOIDCIssuer(_ context.Context, bucket string) error {
	f.deleteCalls++
	f.bucket = bucket

	return f.deleteErr
}

func (f *fakeOIDCIssuerPublisher) PublishOIDCIssuer(_ context.Context, bucket, issuerURL string, publicKeyPEM []byte, keyID string) error {
	f.calls++
	f.bucket = bucket
	f.issuerURL = issuerURL
	f.publicKeyPEM = publicKeyPEM
	f.keyID = keyID

	return f.publishErr
}

func TestConfigReconcileAddsFinalizerWithoutExplicitRequeue(t *testing.T) {
	config := testSelfHostedConfig()
	localClient := testConfigClient(t, config)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Recorder: recorder,
	}

	assertFinalizerAddedOnFirstReconcile(t, localClient, reconciler, config, &identityv1.AWSWorkloadIdentityConfig{}, identityv1.ConfigFinalizer, recorder)
}

func TestConfigReconcileNormalOperatorConfigUnavailablePreservesACKResources(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	ackResources := sentinelACKResources()
	config.Status.ACKResources = ackResources
	localClient := testConfigClient(t, config)
	reconciler := &AWSWorkloadIdentityConfigReconciler{Client: localClient}

	result, err := reconciler.reconcileNormal(ctx, config)
	if err != nil {
		t.Fatalf("expected operator config unavailability to patch status without error, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue, got %#v", result)
	}

	stored := getStoredConfig(t, localClient, config)
	assertACKResources(t, stored.Status.ACKResources, ackResources)
}

func TestConfigReconcileNormalEKSPodIdentityClearsACKResources(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	config.Status.ACKResources = sentinelACKResources()
	localClient := testConfigClient(t, config, testOperatorConfig(), testResolvedClusterProfile(config.Namespace))
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	result, err := reconciler.reconcileNormal(ctx, config)
	if err != nil {
		t.Fatalf("expected EKS Pod Identity reconcile to succeed, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != dependencySteadyStateRequeue {
		t.Fatalf("expected dependency safety requeue %s, got %s", dependencySteadyStateRequeue, result.RequeueAfter)
	}

	stored := getStoredConfig(t, localClient, config)
	if len(stored.Status.ACKResources) != 0 {
		t.Fatalf("expected EKS Pod Identity config to clear ACKResources, got %#v", stored.Status.ACKResources)
	}
}

func TestReconcileSelfHostedIssuerPublishesOIDCObjectsAfterBucketSync(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	bucketName := testIssuerBucketName(config)
	publisher := &fakeOIDCIssuerPublisher{}

	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true))

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 1 {
		t.Fatalf("expected one publish call, got %d", publisher.calls)
	}

	if publisher.bucket != bucketName || publisher.issuerURL != "https://"+bucketName+".s3.ap-northeast-1.amazonaws.com" {
		t.Fatalf("unexpected publisher target: bucket=%q issuer=%q", publisher.bucket, publisher.issuerURL)
	}

	if len(publisher.publicKeyPEM) == 0 || publisher.keyID == "" {
		t.Fatalf("publisher did not receive signing key material")
	}

	secret := &corev1.Secret{}
	if err := reconciler.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: identityaws.SigningKeySecretName(config)}, secret); err != nil {
		t.Fatal(err)
	}

	if secret.Annotations[identityv1.AnnotationSigningKeyID] != publisher.keyID {
		t.Fatalf("expected publisher key ID to match signing Secret annotation, got %q and %q", publisher.keyID, secret.Annotations[identityv1.AnnotationSigningKeyID])
	}

	if !meta.IsStatusConditionTrue(config.Status.Conditions, identityv1.ConditionOIDCObjectsPublished) {
		t.Fatalf("expected OIDCObjectsPublished=True, got %#v", config.Status.Conditions)
	}

	provider := &iamv1alpha1.OpenIDConnectProvider{}
	if err := reconciler.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: identityaws.OIDCProviderName(config)}, provider); err != nil {
		t.Fatal(err)
	}

	if got := provider.Spec.Thumbprints; len(got) != 0 {
		t.Fatalf("expected OIDC provider thumbprints to be omitted, got %#v", got)
	}
}

func TestReconcileSelfHostedIssuerSkipsRepublishWhenKeyIDUnchanged(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true))

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 1 {
		t.Fatalf("expected first reconcile to publish once, got %d", publisher.calls)
	}

	if config.Status.PublishedKeyID == "" {
		t.Fatal("expected PublishedKeyID to be recorded after publish")
	}

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 1 {
		t.Fatalf("expected second reconcile to skip publish, got %d", publisher.calls)
	}
}

func TestEnsureSigningKeyIDRepairsStaleAnnotation(t *testing.T) {
	_, publicKey, keyID, err := oidc.GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				identityv1.AnnotationSigningKeyID: "stale-key-id",
			},
		},
		Data: map[string][]byte{
			identityaws.SigningKeyPublicKey: publicKey,
		},
	}

	if err := ensureSigningKeyID(secret); err != nil {
		t.Fatal(err)
	}

	if got := secret.Annotations[identityv1.AnnotationSigningKeyID]; got != keyID {
		t.Fatalf("expected signing key ID to be repaired to %q, got %q", keyID, got)
	}
}

func TestReconcileSelfHostedIssuerResetsPublishedKeyIDWhenBucketUnsynced(t *testing.T) {
	config := testSelfHostedConfig()
	config.Status.PublishedKeyID = "previous-key"
	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, false))

	if err := reconciler.reconcileSelfHostedIssuer(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	if config.Status.PublishedKeyID != "" {
		t.Fatalf("expected PublishedKeyID to reset when bucket is not synced, got %q", config.Status.PublishedKeyID)
	}
}

func TestReconcileSelfHostedIssuerSkipsPublishUntilBucketSync(t *testing.T) {
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, false))

	if err := reconciler.reconcileSelfHostedIssuer(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 0 {
		t.Fatalf("expected no publish calls before bucket sync, got %d", publisher.calls)
	}

	if meta.IsStatusConditionTrue(config.Status.Conditions, identityv1.ConditionOIDCObjectsPublished) {
		t.Fatalf("expected OIDCObjectsPublished to be false before bucket sync: %#v", config.Status.Conditions)
	}
}

func TestReconcileSelfHostedIssuerMarksPublishFailure(t *testing.T) {
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{publishErr: errors.New("access denied")}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true))

	if err := reconciler.reconcileSelfHostedIssuer(context.Background(), config); err == nil {
		t.Fatal("expected publish error")
	}

	if publisher.calls != 1 {
		t.Fatalf("expected one publish call, got %d", publisher.calls)
	}

	if !meta.IsStatusConditionFalse(config.Status.Conditions, identityv1.ConditionOIDCObjectsPublished) ||
		!meta.IsStatusConditionFalse(config.Status.Conditions, identityv1.ConditionIssuerReady) {
		t.Fatalf("expected publish and issuer conditions to be false: %#v", config.Status.Conditions)
	}
}

func TestSetConfigReadyConditionRequiresIssuerReady(t *testing.T) {
	config := testSelfHostedConfig()
	resolved := &inventory.Resolution{
		Ready:   true,
		Reason:  identityv1.ReasonResolved,
		Message: "ClusterProfile resolved",
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonWaitingForACK, "waiting")

	setConfigReadyCondition(config, resolved)

	if !meta.IsStatusConditionFalse(config.Status.Conditions, identityv1.ConditionReady) {
		t.Fatalf("expected Ready=False while issuer is not ready: %#v", config.Status.Conditions)
	}
}

func TestSetConfigReadyConditionRequiresWebhookRuntimeReady(t *testing.T) {
	config := testSelfHostedConfig()
	resolved := &inventory.Resolution{
		Ready:   true,
		Reason:  identityv1.ReasonResolved,
		Message: "ClusterProfile resolved",
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionFalse, identityv1.ReasonWaitingForWebhookDeployment, "waiting")

	setConfigReadyCondition(config, resolved)

	condition := meta.FindStatusCondition(config.Status.Conditions, identityv1.ConditionReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != identityv1.ReasonWaitingForWebhookDeployment {
		t.Fatalf("expected Ready=False/WaitingForWebhookDeployment while runtime is not ready: %#v", config.Status.Conditions)
	}
}

func TestSelfHostedIssuerFailureClearsStaleReadyConditions(t *testing.T) {
	config := testSelfHostedConfig()
	resolved := &inventory.Resolution{
		Ready:   true,
		Reason:  identityv1.ReasonResolved,
		Message: "ClusterProfile resolved",
	}

	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	setSelfHostedIssuerFailureCondition(config, errors.New("apply failed"))
	setConfigReadyCondition(config, resolved)

	if !meta.IsStatusConditionFalse(config.Status.Conditions, identityv1.ConditionIssuerReady) ||
		!meta.IsStatusConditionFalse(config.Status.Conditions, identityv1.ConditionReady) {
		t.Fatalf("expected stale ready conditions to be false: %#v", config.Status.Conditions)
	}
}

func TestSetResolvedClusterStatusSetsOnlyReadySelfHostedResolution(t *testing.T) {
	config := testSelfHostedConfig()
	resolved := &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	}

	setResolvedClusterStatus(config, resolved)

	if config.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name to be set, got %q", config.Status.ResolvedClusterName)
	}

	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	setResolvedClusterStatus(config, resolved)

	if config.Status.ResolvedClusterName != "" {
		t.Fatalf("expected non-self-hosted delivery to clear resolved cluster name, got %q", config.Status.ResolvedClusterName)
	}

	config.Spec.Type = identityv1.DeliveryTypeSelfHostedIRSA
	config.Status.ResolvedClusterName = "stale/stale"
	resolved.Ready = false
	setResolvedClusterStatus(config, resolved)

	if config.Status.ResolvedClusterName != "" {
		t.Fatalf("expected unresolved inventory to clear resolved cluster name, got %q", config.Status.ResolvedClusterName)
	}
}

func TestWebhookRuntimeRequeueAfterSeparatesReadinessAndCertificateRenewal(t *testing.T) {
	schedule := webhookRuntimeSchedule{
		ReadinessRetryAfter: transientRequeue,
		CertRenewalAfter:    12 * time.Hour,
	}

	if got := webhookRuntimeRequeueAfter(schedule, false); got != transientRequeue {
		t.Fatalf("expected not-ready runtime to use readiness retry %s, got %s", transientRequeue, got)
	}

	if got := webhookRuntimeRequeueAfter(schedule, true); got != 12*time.Hour {
		t.Fatalf("expected ready runtime to use certificate renewal %s, got %s", 12*time.Hour, got)
	}
}

func TestDeleteSelfHostedChildrenDeletesOIDCObjects(t *testing.T) {
	ctx := context.Background()
	config := &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityv1.DefaultName,
			Namespace: testInventoryNamespace,
		},
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type:   identityv1.DeliveryTypeSelfHostedIRSA,
			Region: "ap-northeast-1",
		},
		Status: identityv1.AWSWorkloadIdentityConfigStatus{
			BucketName: testIssuerBucketNameValue,
		},
	}
	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: testConfigClient(t),
		Scheme: testControllerScheme(t),
		SelfHostedIssuerPublisherFactory: func(context.Context, string) (SelfHostedIssuerPublisher, error) {
			return publisher, nil
		},
	}

	if err := reconciler.deleteSelfHostedChildren(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.deleteCalls != 1 || publisher.bucket != testIssuerBucketNameValue {
		t.Fatalf("expected OIDC issuer objects to be deleted, calls=%d bucket=%q", publisher.deleteCalls, publisher.bucket)
	}
}

func TestRemoteRuntimeSharedByOtherConfigUsesResolvedClusterIndex(t *testing.T) {
	config := testSelfHostedConfig()
	sibling := testSelfHostedSiblingConfig()
	markResolvedClusterFresh(sibling, testResolvedClusterName)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   testConfigClient(t, config, sibling),
		Resolver: inventory.Resolver{Client: testConfigClient(t)},
	}

	shared, err := reconciler.remoteRuntimeSharedByOtherConfig(context.Background(), config, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !shared {
		t.Fatal("expected indexed sibling with same resolved cluster to mark runtime shared")
	}
}

func TestRemoteRuntimeSharedByOtherConfigBlocksOnUnknownSiblingInventory(t *testing.T) {
	config := testSelfHostedConfig()
	sibling := testSelfHostedSiblingConfig()
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   testConfigClient(t, config, sibling),
		Resolver: inventory.Resolver{Client: testConfigClient(t)},
	}

	shared, err := reconciler.remoteRuntimeSharedByOtherConfig(context.Background(), config, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})
	if err == nil {
		t.Fatalf("expected unknown sibling inventory to block cleanup, shared=%t", shared)
	}
}

func TestRemoteRuntimeSharedByOtherConfigBlocksOnStaleResolvedClusterStatus(t *testing.T) {
	config := testSelfHostedConfig()
	sibling := testSelfHostedSiblingConfig()
	sibling.Generation = 2
	sibling.Status.ObservedGeneration = 1
	sibling.Status.ResolvedClusterName = "other/other"
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   testConfigClient(t, config, sibling),
		Resolver: inventory.Resolver{Client: testConfigClient(t)},
	}

	shared, err := reconciler.remoteRuntimeSharedByOtherConfig(context.Background(), config, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})
	if err == nil {
		t.Fatalf("expected stale sibling status to be resolved conservatively and block cleanup, shared=%t", shared)
	}
}

func TestRemoteRuntimeSharedByOtherConfigResolvesDifferentCachedSibling(t *testing.T) {
	config := testSelfHostedConfig()
	sibling := testSelfHostedSiblingConfig()
	markResolvedClusterFresh(sibling, "cached-other/cached-other")
	localClient := testConfigClient(t, config, sibling, testResolvedClusterProfile(testSiblingNamespace))
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Resolver: inventory.Resolver{Client: localClient},
	}

	shared, err := reconciler.remoteRuntimeSharedByOtherConfig(context.Background(), config, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if shared {
		t.Fatal("expected resolvable sibling on a different cluster not to mark runtime shared")
	}
}

func TestRemoteRuntimeSharedByOtherConfigFindsRemappedSibling(t *testing.T) {
	config := testSelfHostedConfig()
	sibling := testSelfHostedSiblingConfig()
	markResolvedClusterFresh(sibling, "cached-other/cached-other")

	profile := testResolvedClusterProfile(testSiblingNamespace)
	profile.Labels = map[string]string{inventory.LabelOCMClusterName: testInventoryNamespace}
	localClient := testConfigClient(t, config, sibling, profile)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Resolver: inventory.Resolver{Client: localClient},
	}

	shared, err := reconciler.remoteRuntimeSharedByOtherConfig(context.Background(), config, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !shared {
		t.Fatal("expected resolved sibling remapped to deleting config cluster to mark runtime shared")
	}
}

func TestDeleteSelfHostedChildrenReturnsOIDCObjectDeleteError(t *testing.T) {
	config := testSelfHostedConfig()
	config.Status.BucketName = testIssuerBucketNameValue
	publisher := &fakeOIDCIssuerPublisher{deleteErr: errors.New("access denied")}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: testConfigClient(t),
		Scheme: testControllerScheme(t),
		SelfHostedIssuerPublisherFactory: func(context.Context, string) (SelfHostedIssuerPublisher, error) {
			return publisher, nil
		},
	}

	if err := reconciler.deleteSelfHostedChildren(context.Background(), config); err == nil {
		t.Fatal("expected delete error")
	}
}

// Regression: observers must see ConditionDeletionBlocked transition
// True->False before the finalizer is removed, otherwise the metric allowlist
// latches True forever. The sentinel finalizer keeps the object visible after
// the controller removes its own.
func TestConfigReconcileDeleteClearsDeletionBlocked(t *testing.T) {
	const sentinelFinalizer = "aws.identity.appthrust.io/test-sentinel"

	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	now := metav1.Now()
	config.DeletionTimestamp = &now
	config.Finalizers = []string{identityv1.ConfigFinalizer, sentinelFinalizer}
	setCondition(
		&config.Status.Conditions,
		config.Generation,
		identityv1.ConditionDeletionBlocked,
		metav1.ConditionTrue,
		identityv1.ReasonRemoteClusterUnavailable,
		"remote cluster previously unreachable",
	)

	localClient := testConfigClient(t, config)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Recorder: recorder,
		Resolver: inventory.Resolver{Client: localClient},
	}

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(config)})
	if err != nil {
		t.Fatalf("expected reconcile delete to succeed once blocking is cleared, got result=%#v err=%v", result, err)
	}

	if !result.IsZero() {
		t.Fatalf("expected no requeue when deletion completes, got %#v", result)
	}

	stored := getStoredConfig(t, localClient, config)

	condition := meta.FindStatusCondition(stored.Status.Conditions, identityv1.ConditionDeletionBlocked)
	if condition == nil {
		t.Fatalf("expected ConditionDeletionBlocked to remain on object so observers see the False transition, got %#v", stored.Status.Conditions)
	}

	if condition.Status != metav1.ConditionFalse || condition.Reason != identityv1.ReasonDeletionUnblocked {
		t.Fatalf("expected ConditionDeletionBlocked=False/%s after unblocking, got status=%s reason=%s message=%q", identityv1.ReasonDeletionUnblocked, condition.Status, condition.Reason, condition.Message)
	}

	if controllerutil.ContainsFinalizer(stored, identityv1.ConfigFinalizer) {
		t.Fatalf("expected controller finalizer %q to be removed after blocking cleared, got %#v", identityv1.ConfigFinalizer, stored.Finalizers)
	}

	if !controllerutil.ContainsFinalizer(stored, sentinelFinalizer) {
		t.Fatalf("test sentinel finalizer should still be present so the unblocked condition is observable; got %#v", stored.Finalizers)
	}
}

func testSelfHostedConfig() *identityv1.AWSWorkloadIdentityConfig {
	return &identityv1.AWSWorkloadIdentityConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: identityv1.GroupVersion.String(),
			Kind:       "AWSWorkloadIdentityConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityv1.DefaultName,
			Namespace: testInventoryNamespace,
			UID:       types.UID(testConfigUID),
		},
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type:   identityv1.DeliveryTypeSelfHostedIRSA,
			Region: "ap-northeast-1",
		},
	}
}

func testSelfHostedSiblingConfig() *identityv1.AWSWorkloadIdentityConfig {
	config := testSelfHostedConfig()
	config.Namespace = testSiblingNamespace
	config.UID = types.UID(testSiblingConfigUID)

	return config
}

func markResolvedClusterFresh(config *identityv1.AWSWorkloadIdentityConfig, clusterName string) {
	config.Status.ObservedGeneration = config.Generation
	config.Status.ResolvedClusterName = clusterName
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionClusterProfileResolved, metav1.ConditionTrue, identityv1.ReasonResolved, "ClusterProfile resolved")
}

func testIssuerBucketName(config *identityv1.AWSWorkloadIdentityConfig) string {
	return identityaws.BucketName(config.Namespace, config.Spec.Region)
}

func testBucket(config *identityv1.AWSWorkloadIdentityConfig, synced bool) *s3v1alpha1.Bucket {
	status := corev1.ConditionFalse
	if synced {
		status = corev1.ConditionTrue
	}

	return &s3v1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: testIssuerBucketName(config), Namespace: config.Namespace},
		Status: s3v1alpha1.BucketStatus{
			Conditions: []*ackv1alpha1.Condition{{
				Type:   ackv1alpha1.ConditionTypeResourceSynced,
				Status: status,
			}},
		},
	}
}

func testConfigReconciler(t *testing.T, publisher SelfHostedIssuerPublisher, objs ...client.Object) *AWSWorkloadIdentityConfigReconciler {
	t.Helper()

	return &AWSWorkloadIdentityConfigReconciler{
		Client: testConfigClient(t, objs...),
		Scheme: testControllerScheme(t),
		SelfHostedIssuerPublisherFactory: func(_ context.Context, region string) (SelfHostedIssuerPublisher, error) {
			if region != "ap-northeast-1" {
				t.Fatalf("unexpected publisher region %q", region)
			}

			return publisher, nil
		},
	}
}

func testConfigClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetUID, IndexAWSServiceAccountRoleByReplicaSetUID).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		Build()
}

func sentinelACKResources() []identityv1.ACKResourceStatus {
	return []identityv1.ACKResourceStatus{{
		APIVersion: "example.test/v1",
		Kind:       "Sentinel",
		Namespace:  testInventoryNamespace,
		Name:       "previous",
	}}
}

func assertACKResources(t *testing.T, actual, expected []identityv1.ACKResourceStatus) {
	t.Helper()

	if !apiequality.Semantic.DeepEqual(actual, expected) {
		t.Fatalf("unexpected ACKResources\nexpected: %#v\nactual:   %#v", expected, actual)
	}
}

func testControllerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		identityv1.AddToScheme,
		eksv1alpha1.AddToScheme,
		iamv1alpha1.AddToScheme,
		s3v1alpha1.AddToScheme,
		clusterinventoryv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	return scheme
}
