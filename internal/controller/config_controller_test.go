package controller

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	eksv1alpha1 "github.com/aws-controllers-k8s/eks-controller/apis/v1alpha1"
	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	s3v1alpha1 "github.com/aws-controllers-k8s/s3-controller/apis/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

type fakeOIDCIssuerPublisher struct {
	bucket      string
	deleteCalls int
	deleteErr   error
	publication oidc.IssuerPublication
	ensureErr   error
	changed     bool
	calls       int
}

const testEKSIRSAOIDCProviderARN = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE"

func (f *fakeOIDCIssuerPublisher) DeleteOIDCIssuer(_ context.Context, bucket string) error {
	f.deleteCalls++
	f.bucket = bucket

	return f.deleteErr
}

func (f *fakeOIDCIssuerPublisher) EnsureOIDCIssuer(_ context.Context, bucket string, publication oidc.IssuerPublication) (bool, error) {
	f.calls++
	f.bucket = bucket
	f.publication = publication

	return f.changed, f.ensureErr
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

func TestReconcileSelfHostedIssuerEnsuresOIDCObjectsAfterBucketSync(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	bucketName := testIssuerBucketName(config)
	publisher := &fakeOIDCIssuerPublisher{}

	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true))

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 1 {
		t.Fatalf("expected one S3 ensure call, got %d", publisher.calls)
	}

	if publisher.bucket != bucketName || publisher.publication.IssuerURL != "https://"+bucketName+".s3.ap-northeast-1.amazonaws.com" {
		t.Fatalf("unexpected publisher target: bucket=%q issuer=%q", publisher.bucket, publisher.publication.IssuerURL)
	}

	if publisher.publication.SigningKeyID == "" || publisher.publication.ObjectSetDigest == "" || len(publisher.publication.Objects) != 2 {
		t.Fatalf("publisher did not receive a complete rendered publication: %#v", publisher.publication)
	}

	secret := &corev1.Secret{}
	if err := reconciler.Get(ctx, client.ObjectKey{Namespace: config.Namespace, Name: identityaws.SigningKeySecretName(config)}, secret); err != nil {
		t.Fatal(err)
	}

	if secret.Annotations[identityv1.AnnotationSigningKeyID] != publisher.publication.SigningKeyID {
		t.Fatalf("expected publisher key ID to match signing Secret annotation, got %q and %q", publisher.publication.SigningKeyID, secret.Annotations[identityv1.AnnotationSigningKeyID])
	}

	assertSelfHostedIssuerPublicationStatus(t, config, bucketName, publisher.publication)

	if config.Status.SelfHostedIssuer.BucketName != bucketName {
		t.Fatalf("expected self-hosted issuer bucket %q, got %q", bucketName, config.Status.SelfHostedIssuer.BucketName)
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

func TestReconcileSelfHostedIssuerAlwaysEnsuresS3WhenPublicationAlreadyRecorded(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true))

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	firstPublication := *config.Status.SelfHostedIssuer.Publication

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 2 {
		t.Fatalf("expected second reconcile to verify S3 despite matching status, got %d ensure calls", publisher.calls)
	}

	if got := config.Status.SelfHostedIssuer.Publication; got == nil || *got != firstPublication {
		t.Fatalf("expected publication status to remain consistent, got %#v want %#v", got, firstPublication)
	}
}

func TestReconcileSelfHostedIssuerRepairsMissingPublicationStatusAfterS3Verification(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	publisher := &fakeOIDCIssuerPublisher{changed: false}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true))

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 1 {
		t.Fatalf("expected one S3 verification, got %d", publisher.calls)
	}

	assertSelfHostedIssuerPublicationStatus(t, config, testIssuerBucketName(config), publisher.publication)
}

func assertSelfHostedIssuerPublicationStatus(t *testing.T, config *identityv1.AWSWorkloadIdentityConfig, bucketName string, publication oidc.IssuerPublication) {
	t.Helper()

	if config.Status.SelfHostedIssuer == nil {
		t.Fatal("expected self-hosted issuer status")
	}

	got := config.Status.SelfHostedIssuer.Publication
	if got == nil {
		t.Fatal("expected self-hosted issuer publication status")
	}

	if got.BucketName != bucketName ||
		got.IssuerURL != publication.IssuerURL ||
		got.SigningKeyID != publication.SigningKeyID ||
		got.ObjectSetDigest != publication.ObjectSetDigest {
		t.Fatalf("unexpected publication status: got %#v publication=%#v bucket=%q", got, publication, bucketName)
	}
}

func TestReconcileSelfHostedIssuerPreservesPreCreatedSigningSecretDataFromUncachedReader(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()

	privateKey, publicKey, keyID, err := oidc.GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	secretKey := client.ObjectKey{Namespace: config.Namespace, Name: identityaws.SigningKeySecretName(config)}
	cachedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace}}
	liveSecret := cachedSecret.DeepCopy()
	liveSecret.Data = map[string][]byte{
		identityaws.SigningKeyPrivateKey: privateKey,
		identityaws.SigningKeyPublicKey:  publicKey,
	}

	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true), cachedSecret)
	reconciler.SigningSecretReader = fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(liveSecret).
		Build()

	if err := reconciler.reconcileSelfHostedIssuer(ctx, config); err != nil {
		t.Fatal(err)
	}

	if publisher.publication.SigningKeyID != keyID {
		t.Fatalf("expected publisher key ID %q, got %q", keyID, publisher.publication.SigningKeyID)
	}

	stored := &corev1.Secret{}
	if err := reconciler.Get(ctx, secretKey, stored); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(stored.Data[identityaws.SigningKeyPrivateKey], privateKey) ||
		!bytes.Equal(stored.Data[identityaws.SigningKeyPublicKey], publicKey) {
		t.Fatalf("expected stored signing Secret data to be preserved")
	}
}

func TestPreserveOIDCProviderLateInitializedFields(t *testing.T) {
	lateInitializedThumbprint := "06b25927c42a721631c1efd9431e648fa62e1e39"
	current := iamv1alpha1.OpenIDConnectProviderSpec{
		Thumbprints: []*string{&lateInitializedThumbprint},
	}
	desired := iamv1alpha1.OpenIDConnectProviderSpec{}

	preserveOIDCProviderLateInitializedFields(&desired, &current)

	if len(desired.Thumbprints) != 1 || *desired.Thumbprints[0] != lateInitializedThumbprint {
		t.Fatalf("expected late-initialized thumbprint to be preserved, got %#v", desired.Thumbprints)
	}

	explicitThumbprint := "1111111111111111111111111111111111111111"
	desired = iamv1alpha1.OpenIDConnectProviderSpec{
		Thumbprints: []*string{&explicitThumbprint},
	}

	preserveOIDCProviderLateInitializedFields(&desired, &current)

	if len(desired.Thumbprints) != 1 || *desired.Thumbprints[0] != explicitThumbprint {
		t.Fatalf("expected explicit desired thumbprint to win, got %#v", desired.Thumbprints)
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

func TestReconcileSelfHostedIssuerClearsPublicationWhenBucketUnsynced(t *testing.T) {
	config := testSelfHostedConfig()
	config.Status.SelfHostedIssuer = &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerStatus{
		BucketName: testIssuerBucketName(config),
		Publication: &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerPublicationStatus{
			BucketName:      testIssuerBucketName(config),
			IssuerURL:       "https://previous.example.test",
			SigningKeyID:    "previous-key",
			ObjectSetDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, false))

	if err := reconciler.reconcileSelfHostedIssuer(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	if publisher.calls != 0 {
		t.Fatalf("expected no S3 ensure calls before bucket sync, got %d", publisher.calls)
	}

	if config.Status.SelfHostedIssuer == nil || config.Status.SelfHostedIssuer.BucketName != testIssuerBucketName(config) {
		t.Fatalf("expected bucket target to remain recorded, got %#v", config.Status.SelfHostedIssuer)
	}

	if config.Status.SelfHostedIssuer.Publication != nil {
		t.Fatalf("expected publication to be cleared while bucket is not synced, got %#v", config.Status.SelfHostedIssuer.Publication)
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
	previousPublication := &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerPublicationStatus{
		BucketName:      testIssuerBucketName(config),
		IssuerURL:       "https://previous.example.test",
		SigningKeyID:    "previous-key",
		ObjectSetDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	config.Status.SelfHostedIssuer = &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerStatus{
		BucketName:  testIssuerBucketName(config),
		Publication: previousPublication.DeepCopy(),
	}
	publisher := &fakeOIDCIssuerPublisher{ensureErr: errors.New("access denied")}
	reconciler := testConfigReconciler(t, publisher, config, testBucket(config, true))

	if err := reconciler.reconcileSelfHostedIssuer(context.Background(), config); err == nil {
		t.Fatal("expected publish error")
	}

	if publisher.calls != 1 {
		t.Fatalf("expected one S3 ensure call, got %d", publisher.calls)
	}

	if config.Status.SelfHostedIssuer == nil || config.Status.SelfHostedIssuer.Publication == nil || *config.Status.SelfHostedIssuer.Publication != *previousPublication {
		t.Fatalf("expected previous publication to be preserved on failure, got %#v", config.Status.SelfHostedIssuer)
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

func TestSetResolvedClusterStatusSetsOnlyReadyAnnotationBasedIRSAResolution(t *testing.T) {
	config := testSelfHostedConfig()
	resolved := &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	}

	setResolvedClusterStatus(config, resolved)

	if config.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name to be set, got %q", config.Status.ResolvedClusterName)
	}

	config.Spec.Type = identityv1.DeliveryTypeEKSIRSA
	config.Status.ResolvedClusterName = ""
	setResolvedClusterStatus(config, resolved)

	if config.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected EKSIRSA resolved cluster name to be set, got %q", config.Status.ResolvedClusterName)
	}

	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	setResolvedClusterStatus(config, resolved)

	if config.Status.ResolvedClusterName != "" {
		t.Fatalf("expected non-IRSA delivery to clear resolved cluster name, got %q", config.Status.ResolvedClusterName)
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
			UID:       types.UID(testConfigUID),
		},
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type:   identityv1.DeliveryTypeSelfHostedIRSA,
			Region: "ap-northeast-1",
		},
		Status: identityv1.AWSWorkloadIdentityConfigStatus{
			SelfHostedIssuer: &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerStatus{
				BucketName: testIssuerBucketNameValue,
			},
		},
	}
	bucket := &s3v1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testIssuerBucketNameValue,
			Namespace: config.Namespace,
		},
	}
	markOwnedByConfig(t, config, bucket)

	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: testConfigClient(t, bucket),
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

func TestDeleteSelfHostedChildrenSkipsOIDCObjectsWhenBucketForeign(t *testing.T) {
	config := testSelfHostedConfig()
	config.Status.SelfHostedIssuer = &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerStatus{BucketName: testIssuerBucketNameValue}

	foreign := &s3v1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testIssuerBucketNameValue,
			Namespace: config.Namespace,
		},
	}

	publisher := &fakeOIDCIssuerPublisher{}
	localClient := testConfigClient(t, foreign)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Recorder: recorder,
		SelfHostedIssuerPublisherFactory: func(context.Context, string) (SelfHostedIssuerPublisher, error) {
			return publisher, nil
		},
	}

	entries := []capturedInfoLogEntry{}
	ctx := logr.NewContext(context.Background(), logr.New(captureInfoLogSink{entries: &entries}))

	if err := reconciler.deleteSelfHostedChildren(ctx, config); err != nil {
		t.Fatalf("expected foreign bucket to short-circuit OIDC object deletion without error, got %v", err)
	}

	if publisher.deleteCalls != 0 {
		t.Fatalf("expected publisher not to be invoked for foreign bucket, deleteCalls=%d", publisher.deleteCalls)
	}

	if err := localClient.Get(ctx, client.ObjectKeyFromObject(foreign), &s3v1alpha1.Bucket{}); err != nil {
		t.Fatalf("expected foreign Bucket to be left alone, got %v", err)
	}

	var skipEntry *capturedInfoLogEntry

	for i := range entries {
		if entries[i].msg == "skipping delete: S3 issuer objects target bucket not controlled by this AWSWorkloadIdentityConfig" {
			skipEntry = &entries[i]

			break
		}
	}

	if skipEntry == nil {
		t.Fatalf("expected S3 issuer objects ownership-mismatch skip to be logged via Info; got entries=%#v", entries)
	}

	// deleteSelfHostedChildren issues TWO ownership-guarded operations against
	// the bucket: deleteSelfHostedOIDCObjects (S3 issuer objects) and
	// deleteConfigChildIfOwned (the Bucket CR itself). Both must surface the
	// foreign owner via an Event so operators see the block in
	// `kubectl describe`.
	assertAllEventsForeignChildSkipped(t, recorder.events, config)
}

func TestDeleteSelfHostedChildrenSkipsOIDCObjectsWhenBucketMissing(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	config.Status.SelfHostedIssuer = &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerStatus{BucketName: testIssuerBucketNameValue}

	publisher := &fakeOIDCIssuerPublisher{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: testConfigClient(t),
		Scheme: testControllerScheme(t),
		SelfHostedIssuerPublisherFactory: func(context.Context, string) (SelfHostedIssuerPublisher, error) {
			return publisher, nil
		},
	}

	if err := reconciler.deleteSelfHostedChildren(ctx, config); err != nil {
		t.Fatalf("expected missing bucket to short-circuit OIDC object deletion without error, got %v", err)
	}

	if publisher.deleteCalls != 0 {
		t.Fatalf("expected publisher not to be invoked when bucket is absent, deleteCalls=%d", publisher.deleteCalls)
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
	config.Status.SelfHostedIssuer = &identityv1.AWSWorkloadIdentityConfigSelfHostedIssuerStatus{BucketName: testIssuerBucketNameValue}
	bucket := &s3v1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testIssuerBucketNameValue,
			Namespace: config.Namespace,
		},
	}
	markOwnedByConfig(t, config, bucket)

	publisher := &fakeOIDCIssuerPublisher{deleteErr: errors.New("access denied")}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: testConfigClient(t, bucket),
		Scheme: testControllerScheme(t),
		SelfHostedIssuerPublisherFactory: func(context.Context, string) (SelfHostedIssuerPublisher, error) {
			return publisher, nil
		},
	}

	if err := reconciler.deleteSelfHostedChildren(context.Background(), config); err == nil {
		t.Fatal("expected delete error")
	}
}

func TestDeleteSelfHostedChildrenSkipsForeignSigningSecret(t *testing.T) {
	config := testSelfHostedConfig()
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityaws.SigningKeySecretName(config),
			Namespace: config.Namespace,
		},
	}

	publisher := &fakeOIDCIssuerPublisher{}
	localClient := testConfigClient(t, foreign)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Recorder: recorder,
		SelfHostedIssuerPublisherFactory: func(context.Context, string) (SelfHostedIssuerPublisher, error) {
			return publisher, nil
		},
	}

	entries := []capturedInfoLogEntry{}
	ctx := logr.NewContext(context.Background(), logr.New(captureInfoLogSink{entries: &entries}))

	if err := reconciler.deleteSelfHostedChildren(ctx, config); err != nil {
		t.Fatal(err)
	}

	stillThere := &corev1.Secret{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(foreign), stillThere); err != nil {
		t.Fatalf("expected foreign signing Secret to be left alone, got %v", err)
	}

	var skipEntry *capturedInfoLogEntry

	for i := range entries {
		if entries[i].msg == msgSkippingForeignConfigChild {
			skipEntry = &entries[i]

			break
		}
	}

	if skipEntry == nil {
		t.Fatalf("expected ownership-mismatch skip to be logged via Info; got entries=%#v", entries)
	}

	assertForeignChildSkippedEvent(t, recorder.events, config)
}

func TestDeleteSelfHostedChildrenDeletesOwnedSigningSecret(t *testing.T) {
	config := testSelfHostedConfig()
	owned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityaws.SigningKeySecretName(config),
			Namespace: config.Namespace,
		},
	}
	markOwnedByConfig(t, config, owned)

	publisher := &fakeOIDCIssuerPublisher{}
	localClient := testConfigClient(t, owned)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: localClient,
		Scheme: testControllerScheme(t),
		SelfHostedIssuerPublisherFactory: func(context.Context, string) (SelfHostedIssuerPublisher, error) {
			return publisher, nil
		},
	}

	if err := reconciler.deleteSelfHostedChildren(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(owned), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected owned signing Secret to be deleted, got %v", err)
	}
}

// Regression: when an OpenIDConnectProvider sharing the deterministic
// EKSIRSA name exists with a foreign owner (no controllerRef + no LabelConfigUID
// pointing to this config), deleteEKSIRSAChildren must (a) leave the foreign CR
// alone and (b) emit a structured Info log naming the skipped child so operators
// can audit ownership-mismatch silent skips.
func TestDeleteEKSIRSAChildrenSkipsForeignOpenIDConnectProviderAndLogs(t *testing.T) {
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")

	foreign := &iamv1alpha1.OpenIDConnectProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityaws.OIDCProviderName(config),
			Namespace: config.Namespace,
		},
	}

	localClient := testConfigClient(t, foreign)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Recorder: recorder,
	}

	entries := []capturedInfoLogEntry{}
	ctx := logr.NewContext(context.Background(), logr.New(captureInfoLogSink{entries: &entries}))

	if err := reconciler.deleteEKSIRSAChildren(ctx, config); err != nil {
		t.Fatalf("expected foreign OpenIDConnectProvider to short-circuit deletion without error, got %v", err)
	}

	stillThere := &iamv1alpha1.OpenIDConnectProvider{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(foreign), stillThere); err != nil {
		t.Fatalf("expected foreign OpenIDConnectProvider to be left alone, got %v", err)
	}

	var skipEntry *capturedInfoLogEntry

	for i := range entries {
		if entries[i].msg == msgSkippingForeignConfigChild {
			skipEntry = &entries[i]

			break
		}
	}

	if skipEntry == nil {
		t.Fatalf("expected ownership-mismatch skip to be logged via Info; got entries=%#v", entries)
	}

	assertLogValue(t, skipEntry.values, "awio.child.kind", "*v1alpha1.OpenIDConnectProvider")
	assertLogValue(t, skipEntry.values, "awio.child.namespace", config.Namespace)
	assertLogValue(t, skipEntry.values, "awio.child.name", identityaws.OIDCProviderName(config))

	assertForeignChildSkippedEvent(t, recorder.events, config)
}

// Regression: an OpenIDConnectProvider owned by this config must still be
// deleted by deleteEKSIRSAChildren so the observability change does not
// silently regress the happy-path delete.
func TestDeleteEKSIRSAChildrenDeletesOwnedOpenIDConnectProvider(t *testing.T) {
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")

	owned := &iamv1alpha1.OpenIDConnectProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityaws.OIDCProviderName(config),
			Namespace: config.Namespace,
		},
	}
	markOwnedByConfig(t, config, owned)

	localClient := testConfigClient(t, owned)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: localClient,
		Scheme: testControllerScheme(t),
	}

	if err := reconciler.deleteEKSIRSAChildren(context.Background(), config); err != nil {
		t.Fatalf("expected owned OpenIDConnectProvider deletion to succeed, got %v", err)
	}

	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(owned), &iamv1alpha1.OpenIDConnectProvider{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected owned OpenIDConnectProvider to be deleted, got %v", err)
	}
}

// Regression: external-managed configs do not own an OpenIDConnectProvider CR,
// so deleteEKSIRSAChildren must short-circuit without Get/Delete (and without
// emitting the ownership-mismatch skip log) even if a foreign CR sharing the
// deterministic name happens to exist on the cluster.
func TestDeleteEKSIRSAChildrenSkipsWhenProviderNotManaged(t *testing.T) {
	config := testEKSIRSAConfig(
		identityv1.OIDCProviderManagementExternal,
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	)

	foreign := &iamv1alpha1.OpenIDConnectProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityaws.OIDCProviderName(config),
			Namespace: config.Namespace,
		},
	}

	localClient := testConfigClient(t, foreign)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: localClient,
		Scheme: testControllerScheme(t),
	}

	entries := []capturedInfoLogEntry{}
	ctx := logr.NewContext(context.Background(), logr.New(captureInfoLogSink{entries: &entries}))

	if err := reconciler.deleteEKSIRSAChildren(ctx, config); err != nil {
		t.Fatalf("expected externally-managed config to short-circuit deletion without error, got %v", err)
	}

	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(foreign), &iamv1alpha1.OpenIDConnectProvider{}); err != nil {
		t.Fatalf("expected foreign OpenIDConnectProvider to be left alone, got %v", err)
	}

	for _, e := range entries {
		if e.msg == msgSkippingForeignConfigChild {
			t.Fatalf("did not expect ownership-mismatch skip log when provider is not operator-managed; entry=%#v", e)
		}
	}
}

// Regression: when the IAM ACK OpenIDConnectProvider CRD is not
// installed on the cluster (e.g. the IAM controller chart was never installed
// or was uninstalled), the API server's RESTMapper surfaces a
// meta.NoMatchError on Get/Delete. deleteEKSIRSAChildren must tolerate that
// alongside NotFound and return nil so finalizer removal can proceed.
// Previously the code only tolerated NotFound via client.IgnoreNotFound and
// would surface the NoMatchError, latching deletion forever.
func TestDeleteEKSIRSAChildrenIgnoresMissingOpenIDConnectProviderCRD(t *testing.T) {
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")

	// The fake client returns a runtime "no kind registered" error when the
	// scheme lacks a type rather than the meta.NoMatchError a real RESTMapper
	// would produce. Intercept Get/Delete on OpenIDConnectProvider to return a
	// real meta.NoMatchError so we exercise the exact production branch added
	// for missing-CRD detection.
	noMatchErr := &meta.NoKindMatchError{
		GroupKind: iamv1alpha1.GroupVersion.WithKind("OpenIDConnectProvider").GroupKind(),
	}

	localClient := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*iamv1alpha1.OpenIDConnectProvider); ok {
					return noMatchErr
				}

				return c.Get(ctx, key, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*iamv1alpha1.OpenIDConnectProvider); ok {
					return noMatchErr
				}

				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: localClient,
		Scheme: testControllerScheme(t),
	}

	if err := reconciler.deleteEKSIRSAChildren(context.Background(), config); err != nil {
		t.Fatalf("expected deleteEKSIRSAChildren to tolerate missing OpenIDConnectProvider CRD, got %v", err)
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

func TestReconcileNormalEKSIRSAManagedCreatesProviderWithoutSelfHostedResources(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")
	providerARN := testEKSIRSAOIDCProviderARN
	localClient := testConfigClient(
		t,
		config,
		testOperatorConfig(),
		testResolvedClusterProfile(config.Namespace),
		testOIDCProvider(config, providerARN),
	)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	result, err := reconciler.reconcileNormal(ctx, config)
	if err != nil {
		t.Fatalf("expected EKSIRSA managed reconcile to succeed, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != dependencySteadyStateRequeue {
		t.Fatalf("expected dependency safety requeue %s, got %#v", dependencySteadyStateRequeue, result)
	}

	stored := getStoredConfig(t, localClient, config)
	assertManagedEKSIRSAConfigStatus(t, stored, providerARN)
	assertManagedEKSIRSAProvider(t, localClient, config)
	assertSelfHostedConfigResourcesAbsent(t, localClient, config)
}

func assertManagedEKSIRSAConfigStatus(t *testing.T, stored *identityv1.AWSWorkloadIdentityConfig, providerARN string) {
	t.Helper()

	if stored.Status.IssuerHostPath != "oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE" {
		t.Fatalf("unexpected issuer host/path %q", stored.Status.IssuerHostPath)
	}

	if stored.Status.OIDCProviderARN != providerARN {
		t.Fatalf("expected managed provider ARN from ACK status, got %q", stored.Status.OIDCProviderARN)
	}

	if stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected EKSIRSA to retain resolved cluster name %q, got %q", testResolvedClusterName, stored.Status.ResolvedClusterName)
	}

	if stored.Status.SelfHostedIssuer != nil ||
		stored.Status.WebhookRuntimeNamespace != "" ||
		stored.Status.WebhookRuntimeCertNotAfter != nil {
		t.Fatalf("expected self-hosted status fields to stay empty, got %#v", stored.Status)
	}

	if len(stored.Status.ACKResources) != 1 || stored.Status.ACKResources[0].Kind != ackChildKindOpenIDConnectProvider {
		t.Fatalf("expected only managed OIDC provider ACKResource, got %#v", stored.Status.ACKResources)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIAMProviderReady, metav1.ConditionTrue, identityv1.ReasonACKResourceSynced)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, identityv1.ReasonNotRequired)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReconciled)
}

func assertManagedEKSIRSAProvider(t *testing.T, localClient client.Client, config *identityv1.AWSWorkloadIdentityConfig) {
	t.Helper()

	provider := &iamv1alpha1.OpenIDConnectProvider{ObjectMeta: metav1.ObjectMeta{Name: identityaws.OIDCProviderName(config), Namespace: config.Namespace}}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(provider), provider); err != nil {
		t.Fatal(err)
	}

	if provider.Spec.URL == nil || *provider.Spec.URL != "https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE" {
		t.Fatalf("unexpected provider URL %#v", provider.Spec.URL)
	}

	if len(provider.Spec.ClientIDs) != 1 || *provider.Spec.ClientIDs[0] != remoteirsa.STSAudience {
		t.Fatalf("unexpected provider audiences %#v", provider.Spec.ClientIDs)
	}
}

func assertSelfHostedConfigResourcesAbsent(t *testing.T, localClient client.Client, config *identityv1.AWSWorkloadIdentityConfig) {
	t.Helper()

	signingSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: identityaws.SigningKeySecretName(config), Namespace: config.Namespace}}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(signingSecret), signingSecret); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no signing Secret, got %v", err)
	}

	bucket := &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: testIssuerBucketName(config), Namespace: config.Namespace}}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(bucket), bucket); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no S3 Bucket, got %v", err)
	}
}

func TestReconcileNormalEKSIRSAExternalUsesSpecARNWithoutACKProvider(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	providerARN := testEKSIRSAOIDCProviderARN
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementExternal, providerARN)
	localClient := testConfigClient(t, config, testOperatorConfig(), testResolvedClusterProfile(config.Namespace))
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if result, err := reconciler.reconcileNormal(ctx, config); err != nil {
		t.Fatalf("expected EKSIRSA external reconcile to succeed, got result=%#v err=%v", result, err)
	}

	stored := getStoredConfig(t, localClient, config)
	if stored.Status.OIDCProviderARN != providerARN {
		t.Fatalf("expected external provider ARN from spec, got %q", stored.Status.OIDCProviderARN)
	}

	if stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected EKSIRSA to retain resolved cluster name %q, got %q", testResolvedClusterName, stored.Status.ResolvedClusterName)
	}

	if len(stored.Status.ACKResources) != 0 {
		t.Fatalf("expected no operator-owned ACK resources for external provider, got %#v", stored.Status.ACKResources)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIAMProviderReady, metav1.ConditionTrue, identityv1.ReasonReady)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionWebhookRuntimeReady, metav1.ConditionTrue, identityv1.ReasonNotRequired)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReconciled)

	provider := &iamv1alpha1.OpenIDConnectProvider{ObjectMeta: metav1.ObjectMeta{Name: identityaws.OIDCProviderName(config), Namespace: config.Namespace}}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(provider), provider); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no managed OIDC provider ACK CR, got %v", err)
	}
}

func TestReconcileNormalEKSIRSAExternalRejectsMismatchedProviderARN(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testEKSIRSAConfig(
		identityv1.OIDCProviderManagementExternal,
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/OTHER",
	)
	localClient := testConfigClient(t, config, testOperatorConfig(), testResolvedClusterProfile(config.Namespace))
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if result, err := reconciler.reconcileNormal(ctx, config); err != nil {
		t.Fatalf("expected invalid external provider to be reported in status, got result=%#v err=%v", result, err)
	}

	stored := getStoredConfig(t, localClient, config)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIAMProviderReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)

	if stored.Status.OIDCProviderARN != "" {
		t.Fatalf("expected invalid external provider ARN not to be published into status, got %q", stored.Status.OIDCProviderARN)
	}
}

func TestReconcileNormalEKSIRSAManagedProviderApplyFailureClearsStaleReadyStatus(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")
	config.Status.IssuerHostPath = "stale.example.test/id/OLD"
	config.Status.OIDCProviderARN = "arn:aws:iam::123456789012:oidc-provider/stale.example.test/id/OLD"
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIAMProviderReady, metav1.ConditionTrue, identityv1.ReasonACKResourceSynced, "stale")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady, "stale")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReconciled, "stale")

	applyErr := errors.New("simulated provider apply failure")
	localClient := testConfigClientFailingOnOIDCProviderCreate(t, config, applyErr)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	result, err := reconciler.reconcileNormal(ctx, config)
	if err == nil {
		t.Fatalf("expected provider apply failure, got result=%#v err=nil", result)
	}

	stored := getStoredConfig(t, localClient, config)
	if stored.Status.OIDCProviderARN != "" {
		t.Fatalf("expected stale provider ARN to be cleared on apply failure, got %q", stored.Status.OIDCProviderARN)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIAMProviderReady, metav1.ConditionFalse, identityv1.ReasonIssuerReconcileFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonIssuerReconcileFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonIssuerReconcileFailed)
}

func TestReconcileNormalEKSIRSANilSpecClearsStaleIssuerStatus(t *testing.T) {
	ctx := logr.NewContext(context.Background(), logr.Discard())
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")
	config.Spec.EKSIRSA = nil
	config.Status.IssuerHostPath = "stale.example.test/id/OLD"
	config.Status.OIDCProviderARN = "arn:aws:iam::123456789012:oidc-provider/stale.example.test/id/OLD"
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIAMProviderReady, metav1.ConditionTrue, identityv1.ReasonACKResourceSynced, "stale")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionIssuerReady, metav1.ConditionTrue, identityv1.ReasonReady, "stale")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReconciled, "stale")
	localClient := testConfigClient(t, config, testOperatorConfig(), testResolvedClusterProfile(config.Namespace))
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if result, err := reconciler.reconcileNormal(ctx, config); err != nil {
		t.Fatalf("expected nil EKSIRSA spec to be reported in status, got result=%#v err=%v", result, err)
	}

	stored := getStoredConfig(t, localClient, config)
	if stored.Status.IssuerHostPath != "" || stored.Status.OIDCProviderARN != "" {
		t.Fatalf("expected stale issuer/provider status to be cleared, got hostPath=%q arn=%q", stored.Status.IssuerHostPath, stored.Status.OIDCProviderARN)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIAMProviderReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionIssuerReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
}

func TestDeleteConfigChildrenHandlesEKSIRSAManagedAndExternal(t *testing.T) {
	managed := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")
	managedProvider := testOIDCProvider(managed, testEKSIRSAOIDCProviderARN)
	markOwnedByConfig(t, managed, managedProvider)

	externalARN := testEKSIRSAOIDCProviderARN
	external := testEKSIRSAConfig(identityv1.OIDCProviderManagementExternal, externalARN)
	external.Namespace = "wlc-external"
	externalProvider := testOIDCProvider(external, externalARN)

	localClient := testConfigClient(t, managed, managedProvider, external, externalProvider)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: localClient,
		Scheme: testControllerScheme(t),
	}

	if err := reconciler.deleteConfigChildren(context.Background(), managed); err != nil {
		t.Fatal(err)
	}

	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(managedProvider), &iamv1alpha1.OpenIDConnectProvider{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected managed OIDC provider to be deleted, got %v", err)
	}

	if err := reconciler.deleteConfigChildren(context.Background(), external); err != nil {
		t.Fatal(err)
	}

	stillThere := &iamv1alpha1.OpenIDConnectProvider{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(externalProvider), stillThere); err != nil {
		t.Fatalf("expected external provider ACK CR to be left alone, got %v", err)
	}
}

func TestDeleteConfigChildrenHandlesEKSIRSAUnownedProvider(t *testing.T) {
	config := testEKSIRSAConfig(identityv1.OIDCProviderManagementManaged, "")
	provider := testOIDCProvider(config, testEKSIRSAOIDCProviderARN)

	localClient := testConfigClient(t, config, provider)
	reconciler := &AWSWorkloadIdentityConfigReconciler{
		Client: localClient,
		Scheme: testControllerScheme(t),
	}

	if err := reconciler.deleteConfigChildren(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	stillThere := &iamv1alpha1.OpenIDConnectProvider{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(provider), stillThere); err != nil {
		t.Fatalf("expected unowned provider ACK CR to be left alone, got %v", err)
	}
}

func testEKSIRSAConfig(management identityv1.OIDCProviderManagement, arn string) *identityv1.AWSWorkloadIdentityConfig {
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
			Type:   identityv1.DeliveryTypeEKSIRSA,
			Region: "ap-northeast-1",
			EKSIRSA: &identityv1.EKSIRSAConfig{
				IssuerURL: "https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
				OIDCProvider: identityv1.EKSIRSAOIDCProviderConfig{
					Management: management,
					ARN:        arn,
				},
			},
		},
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

func testConfigClientFailingOnOIDCProviderCreate(t *testing.T, config *identityv1.AWSWorkloadIdentityConfig, createErr error) client.WithWatch {
	t.Helper()

	operatorConfig := testOperatorConfig()
	clusterProfile := testResolvedClusterProfile(config.Namespace)

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(config, operatorConfig, clusterProfile).
		WithStatusSubresource(config, operatorConfig, clusterProfile).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*iamv1alpha1.OpenIDConnectProvider); ok {
					return createErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
}

func testConfigClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSServiceAccountRoleReplicaSet{}, IndexReplicaSetByPlacementRef, IndexAWSServiceAccountRoleReplicaSetByPlacementRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		Build()
}

func markOwnedByConfig(t *testing.T, config *identityv1.AWSWorkloadIdentityConfig, obj client.Object) {
	t.Helper()

	obj.SetLabels(identityaws.LabelsForConfig(config))

	if err := controllerutil.SetControllerReference(config, obj, testControllerScheme(t)); err != nil {
		t.Fatal(err)
	}
}

// msgSkippingForeignConfigChild mirrors the Info log emitted by
// deleteConfigChildIfOwned when ownership-guard short-circuits a child delete.
// Production code in config_controller.go keeps the matching literal as part of
// the operator's structured-log contract; tests reference this const only to
// avoid goconst churn across the three regression sites.
const msgSkippingForeignConfigChild = "skipping delete: child is not controlled by this AWSWorkloadIdentityConfig"

// assertAllEventsForeignChildSkipped verifies recorder captured at least one
// foreign-child skip Warning event for parent, and that EVERY recorded event
// matches the foreign-skip signature. Used by ownership-guard skip paths that
// legitimately emit multiple events for the same foreign object (e.g.,
// deleteSelfHostedChildren issues both deleteSelfHostedOIDCObjects and
// deleteConfigChildIfOwned against the bucket).
func assertAllEventsForeignChildSkipped(t *testing.T, recorded []recordedEvent, parent *identityv1.AWSWorkloadIdentityConfig) {
	t.Helper()

	if len(recorded) == 0 {
		t.Fatalf("expected at least one ChildOwnershipMismatch event, got none")
	}

	for i, ev := range recorded {
		if ev.regarding != parent {
			t.Fatalf("event %d: expected regarding=config, got %#v", i, ev.regarding)
		}

		if ev.eventType != corev1.EventTypeWarning ||
			ev.reason != identityv1.ReasonChildOwnershipMismatch ||
			ev.action != eventActionSkipDeleteForeignChild {
			t.Fatalf("event %d: expected Warning/%s/%s, got %s/%s/%s",
				i, identityv1.ReasonChildOwnershipMismatch, eventActionSkipDeleteForeignChild,
				ev.eventType, ev.reason, ev.action)
		}
	}
}

// assertForeignChildSkippedEvent verifies recorder captured exactly one
// foreign-child skip Warning event for parent. Used by tests covering the
// ownership-guard skip paths in deleteConfigChildIfOwned / deleteEKSIRSAChildren
// (ownership-mismatch observability).
func assertForeignChildSkippedEvent(t *testing.T, recorded []recordedEvent, parent *identityv1.AWSWorkloadIdentityConfig) {
	t.Helper()

	if len(recorded) != 1 {
		t.Fatalf("expected exactly one ChildOwnershipMismatch event, got %d: %#v", len(recorded), recorded)
	}

	ev := recorded[0]
	if ev.regarding != parent {
		t.Fatalf("expected event regarding=parent config, got %#v", ev.regarding)
	}

	if ev.eventType != corev1.EventTypeWarning {
		t.Fatalf("expected Warning event, got %q", ev.eventType)
	}

	if ev.reason != identityv1.ReasonChildOwnershipMismatch {
		t.Fatalf("expected reason=%q, got %q", identityv1.ReasonChildOwnershipMismatch, ev.reason)
	}

	if ev.action != eventActionSkipDeleteForeignChild {
		t.Fatalf("expected action=%q, got %q", eventActionSkipDeleteForeignChild, ev.action)
	}
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
		clusterv1beta1.Install,
		clusterinventoryv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	return scheme
}
