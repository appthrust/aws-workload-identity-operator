package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

const (
	webhookComponentName = "pod-identity-webhook"
	webhookCASecretName  = webhookComponentName + "-ca"

	// labelAppName is the k8s.io/recommended-labels "name" key used as the
	// Service.Spec.Selector and Pod label tying the webhook Service to its
	// Deployment.
	labelAppName = "app.kubernetes.io/name"

	// webhookAdmissionName is the MutatingWebhookConfiguration's webhook entry
	// name. It must be a DNS subdomain because it surfaces in API server audit
	// logs and admission metrics.
	webhookAdmissionName = "pod-identity-webhook.amazonaws.com"

	// DefaultPodIdentityWebhookImage is the upstream aws-pod-identity-webhook image
	// applied to managed workload clusters when no override is supplied.
	DefaultPodIdentityWebhookImage = "public.ecr.aws/eks/amazon-eks-pod-identity-webhook:v0.6.15"

	webhookRuntimeUserID int64 = 65532

	// webhookCertValidity is how long a freshly-generated self-signed leaf cert
	// stays valid before needing rotation.
	webhookCertValidity = 365 * 24 * time.Hour

	// webhookCertRenewalThreshold is the headroom before NotAfter inside which
	// the reuse check forces regeneration. 30 days gives the reconcile loop
	// ample time to complete renewal before the existing cert would expire.
	webhookCertRenewalThreshold = 30 * 24 * time.Hour

	// webhookRSAKeyBits is the RSA modulus size for the self-signed serving cert.
	// 2048 is the minimum currently considered secure; bump to 3072 for FIPS.
	webhookRSAKeyBits = 2048

	// webhookCertClockSkew is subtracted from NotBefore to absorb clock skew
	// between the API server and the workload Pod hosting the webhook.
	webhookCertClockSkew = 5 * time.Minute

	webhookCertRequeueMinimum = time.Minute

	// webhookCertRequeueMaximum bounds how far ahead the cert-renewal requeue
	// can schedule. Cert renewal still wakes via runtime drift watches; the cap
	// preserves a steady cadence even if RequeueAfter is dropped during
	// controller restarts or workqueue eviction.
	webhookCertRequeueMaximum = 24 * time.Hour

	webhookCACertKey                 = "ca.crt"
	webhookCAKeyKey                  = "ca.key"
	webhookServingCertFingerprintKey = "aws.identity.appthrust.io/serving-cert-sha256"
	webhookSkipLabelValue            = "true"
)

type webhookRuntimeObservation struct {
	WebhookNamespace string
	CertNotAfter     metav1.Time
}

type webhookRuntimeCondition struct {
	Ready   bool
	Reason  string
	Message string
}

type webhookDeploymentReadiness struct {
	Ready   bool
	Reason  string
	Message string
}

type webhookRuntimeSchedule struct {
	ReadinessRetryAfter time.Duration
	CertRenewalAfter    time.Duration
}

type webhookRuntimeOutcome struct {
	Observation webhookRuntimeObservation
	Condition   webhookRuntimeCondition
	Schedule    webhookRuntimeSchedule
}

type selfHostedWebhookRuntimeWatchSpec struct {
	newObject   func() client.Object
	cacheScoped bool
}

var selfHostedWebhookRuntimeWatchSpecs = []selfHostedWebhookRuntimeWatchSpec{
	{newObject: func() client.Object { return &corev1.Namespace{} }, cacheScoped: true},
	{newObject: func() client.Object { return &corev1.Secret{} }, cacheScoped: true},
	{newObject: func() client.Object { return &corev1.ServiceAccount{} }},
	// Role/RoleBinding entries remain solely for legacy cleanup of pre-RBAC-04 empty objects (no longer applied).
	{newObject: func() client.Object { return &rbacv1.Role{} }, cacheScoped: true},
	{newObject: func() client.Object { return &rbacv1.RoleBinding{} }, cacheScoped: true},
	{newObject: func() client.Object { return &rbacv1.ClusterRole{} }, cacheScoped: true},
	{newObject: func() client.Object { return &rbacv1.ClusterRoleBinding{} }, cacheScoped: true},
	{newObject: func() client.Object { return &corev1.Service{} }, cacheScoped: true},
	{newObject: func() client.Object { return &appsv1.Deployment{} }, cacheScoped: true},
	{newObject: func() client.Object { return &admissionregistrationv1.MutatingWebhookConfiguration{} }, cacheScoped: true},
}

// SelfHostedWebhookRuntimeReconciler bridges remote runtime drift back to the
// local Config controller. The Config reconciler is the only writer for remote
// runtime resources.
type SelfHostedWebhookRuntimeReconciler struct {
	LocalClient             client.Client
	RuntimeEvents           chan<- event.TypedGenericEvent[*identityv1.AWSWorkloadIdentityConfig]
	MaxConcurrentReconciles int
}

// Reconcile maps managed remote runtime object drift back to the owning local
// AWSWorkloadIdentityConfig/default.
func (r *SelfHostedWebhookRuntimeReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (result ctrl.Result, reconcileErr error) {
	namespace := inventoryNamespaceFromCluster(req.ClusterName.String())
	log := loggerForSelfHostedRemoteRequest(ctx, metrics.ControllerSelfHostedWebhook, req, namespace)
	ctx = logf.IntoContext(ctx, log)
	log.V(1).Info("starting reconcile")

	defer func() {
		logReconcileEnd(log, result, reconcileErr)
	}()

	if namespace == "" {
		metrics.RecordRemoteDelivery("", metrics.ResourceWebhookRuntime, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonNoNamespace)

		return ctrl.Result{}, nil
	}

	config, err := loadSelfHostedConfig(ctx, r.LocalClient, namespace, metrics.ResourceWebhookRuntime, log)
	if err != nil {
		if errors.Is(err, errReconcileDone) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	select {
	case r.RuntimeEvents <- event.TypedGenericEvent[*identityv1.AWSWorkloadIdentityConfig]{Object: config}:
		metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), metrics.ResourceWebhookRuntime, metrics.RemoteDeliveryResultSuccess, metrics.RemoteDeliveryReasonEnqueued)

		return ctrl.Result{}, nil
	case <-ctx.Done():
		return ctrl.Result{}, fmt.Errorf("enqueue runtime event cancelled: %w", ctx.Err())
	default:
		log.V(1).Info("remote runtime event channel full; retrying later")
		metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), metrics.ResourceWebhookRuntime, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonChannelFull)

		return ctrl.Result{RequeueAfter: channelFullRequeue}, nil
	}
}

// applyRemoteWebhookRuntime applies the self-hosted webhook runtime to a
// remote workload cluster. The Config reconciler is the canonical caller;
// remote drift controllers only enqueue the owning Config.
//
// Apply order: Namespace → TLS Secrets → (RBAC + Service in parallel) →
// Deployment/readiness check → MutatingWebhookConfiguration.
//
// The MutatingWebhookConfiguration is applied LAST so that the API server
// only starts routing Pod CREATE admission to the webhook after the
// Deployment reports Available. Existing MWHCs are never deleted just because
// the Deployment is temporarily unavailable.
func applyRemoteWebhookRuntime(ctx context.Context, log logr.Logger, remoteClient client.Client, webhookNamespace, image string) (webhookRuntimeOutcome, error) {
	if webhookNamespace == "" {
		webhookNamespace = identityv1.DefaultSelfHostedWebhookNamespace
	}

	if image == "" {
		image = DefaultPodIdentityWebhookImage
	}

	outcome := webhookRuntimeOutcome{
		Observation: webhookRuntimeObservation{
			WebhookNamespace: webhookNamespace,
		},
		Schedule: webhookRuntimeSchedule{
			ReadinessRetryAfter: transientRequeue,
		},
	}

	op, err := ensureRemoteNamespace(ctx, remoteClient, webhookNamespace)
	logRemoteApply(log, "Namespace", webhookNamespace, op, err)

	if err != nil {
		return outcome, fmt.Errorf("ensure remote Namespace %q: %w", webhookNamespace, err)
	}

	certObservation, caBundle, certFingerprint, err := applyRemoteWebhookCertificates(ctx, log, remoteClient, webhookNamespace)
	if err != nil {
		return outcome, err
	}

	outcome.Observation.CertNotAfter = certObservation.CertNotAfter
	outcome.Schedule.CertRenewalAfter = nextWebhookCertRequeue(certObservation.CertNotAfter)

	if err := applyRemoteSupportingResources(ctx, log, remoteClient, webhookNamespace); err != nil {
		return outcome, err
	}

	deployment, op, err := ensureRemoteDeployment(ctx, remoteClient, webhookNamespace, image, certFingerprint)
	logRemoteApply(log, "Deployment", webhookComponentName, op, err)

	if err != nil {
		return outcome, fmt.Errorf("ensure remote Deployment %q: %w", webhookComponentName, err)
	}

	condition, err := finishRemoteWebhookAdmission(ctx, log, remoteClient, webhookNamespace, caBundle, deployment)
	outcome.Condition = condition

	return outcome, err
}

func applyRemoteWebhookCertificates(ctx context.Context, log logr.Logger, remoteClient client.Client, webhookNamespace string) (webhookRuntimeObservation, []byte, string, error) {
	caOp, caBundle, caCert, caKey, err := ensureRemoteCASecret(ctx, remoteClient, webhookNamespace)
	logRemoteApply(log, "Secret", webhookCASecretName, caOp, err)

	if err != nil {
		return webhookRuntimeObservation{}, nil, "", fmt.Errorf("ensure remote CA Secret %q: %w", webhookCASecretName, err)
	}

	servingOp, certNotAfter, certFingerprint, err := ensureRemoteServingTLSSecret(ctx, remoteClient, webhookNamespace, caCert, caKey)
	logRemoteApply(log, "Secret", webhookComponentName, servingOp, err)

	if err != nil {
		return webhookRuntimeObservation{}, nil, "", fmt.Errorf("ensure remote TLS Secret %q: %w", webhookComponentName, err)
	}

	return webhookRuntimeObservation{CertNotAfter: certNotAfter}, caBundle, certFingerprint, nil
}

func finishRemoteWebhookAdmission(ctx context.Context, log logr.Logger, remoteClient client.Client, webhookNamespace string, caBundle []byte, deployment *appsv1.Deployment) (webhookRuntimeCondition, error) {
	// MWHC is gated on Deployment availability to avoid pointing admission at a
	// Service with no listening Pod; once installed we refresh unconditionally.
	readiness := assessRemoteDeploymentReadiness(deployment)
	if !readiness.Ready {
		// ensureRemoteMutatingWebhookConfiguration below already fetches MWHC via
		// CreateOrUpdate; this Get only earns its keep here, where we may short-circuit
		// before that fetch runs.
		mwhcExists, err := remoteMutatingWebhookConfigurationExists(ctx, remoteClient)
		if err != nil {
			return webhookRuntimeCondition{}, err
		}

		if !mwhcExists {
			return webhookRuntimeCondition{
				Ready:   false,
				Reason:  readiness.Reason,
				Message: "waiting for remote webhook Deployment to become Available before installing admission configuration: " + readiness.Message,
			}, nil
		}
	}

	webhook, op, err := ensureRemoteMutatingWebhookConfiguration(ctx, remoteClient, webhookNamespace, caBundle)
	logRemoteApply(log, "MutatingWebhookConfiguration", webhookComponentName, op, err)

	if err != nil {
		return webhookRuntimeCondition{}, fmt.Errorf("ensure remote MutatingWebhookConfiguration %q: %w", webhookComponentName, err)
	}

	if !readiness.Ready {
		return webhookRuntimeCondition{
			Ready:   false,
			Reason:  readiness.Reason,
			Message: readiness.Message,
		}, nil
	}

	if !mutatingWebhookConfigurationSynced(webhook, webhookNamespace, caBundle) {
		return webhookRuntimeCondition{
			Ready:   false,
			Reason:  identityv1.ReasonWebhookRuntimeUnavailable,
			Message: "remote MutatingWebhookConfiguration is not synced to the current Service and CA bundle",
		}, nil
	}

	return webhookRuntimeCondition{
		Ready:   true,
		Reason:  identityv1.ReasonWebhookRuntimeSynced,
		Message: "remote self-hosted webhook runtime is synced and serving-ready",
	}, nil
}

// applyRemoteSupportingResources fans out the RBAC + Service objects in
// parallel; none of them depend on each other. All children share a single
// metadata.name (webhookComponentName), so the table only carries the kind.
func applyRemoteSupportingResources(ctx context.Context, log logr.Logger, remoteClient client.Client, webhookNamespace string) error {
	parallel := []struct {
		kind  string
		apply func(context.Context, client.Client, string) (controllerutil.OperationResult, error)
	}{
		{kind: "ServiceAccount", apply: ensureRemoteServiceAccount},
		{kind: "ClusterRole", apply: ensureRemoteClusterRole},
		{kind: "ClusterRoleBinding", apply: ensureRemoteClusterRoleBinding},
		{kind: "Service", apply: ensureRemoteService},
	}

	g, gCtx := errgroup.WithContext(ctx)

	for _, step := range parallel {
		g.Go(func() error {
			op, err := step.apply(gCtx, remoteClient, webhookNamespace)
			logRemoteApply(log, step.kind, webhookComponentName, op, err)

			if err != nil {
				return fmt.Errorf("ensure remote %s %q: %w", step.kind, webhookComponentName, err)
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("apply remote webhook runtime: %w", err)
	}

	return nil
}

// SetupWithManager registers the remote runtime drift bridge. It watches only
// managed singleton runtime resources and enqueues the local Config owner
// through RuntimeEvents.
func (r *SelfHostedWebhookRuntimeReconciler) SetupWithManager(mcMgr mcmanager.Manager) error {
	runtimeOnly := webhookRuntimeObjectPredicate()

	enqueue := mchandler.EnqueueRequestForObject
	builder := mcbuilder.ControllerManagedBy(mcMgr).
		Named(metrics.ControllerSelfHostedWebhook).
		WithOptions(mccontroller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles})

	for _, obj := range SelfHostedWebhookRuntimeWatchObjects() {
		builder = builder.Watches(obj, enqueue, mcbuilder.WithPredicates(runtimeOnly))
	}

	if err := builder.Complete(r); err != nil {
		return fmt.Errorf("set up self-hosted webhook runtime controller: %w", err)
	}

	return nil
}

// SelfHostedWebhookRuntimeWatchObjects returns the remote object types watched
// by the self-hosted webhook runtime drift controller.
func SelfHostedWebhookRuntimeWatchObjects() []client.Object {
	objects := make([]client.Object, 0, len(selfHostedWebhookRuntimeWatchSpecs))

	for _, spec := range selfHostedWebhookRuntimeWatchSpecs {
		objects = append(objects, spec.newObject())
	}

	return objects
}

// SelfHostedWebhookRuntimeCacheByObject returns per-GVK cache selectors for
// webhook-runtime-only types. ServiceAccount is intentionally omitted because
// it shares the remote cache with other self-hosted controllers.
func SelfHostedWebhookRuntimeCacheByObject() map[client.Object]cache.ByObject {
	byObject := make(map[client.Object]cache.ByObject)

	for _, spec := range selfHostedWebhookRuntimeWatchSpecs {
		if !spec.cacheScoped {
			continue
		}

		byObject[spec.newObject()] = cache.ByObject{Label: selfHostedWebhookRuntimeCacheSelector()}
	}

	return byObject
}

// SelfHostedWebhookRuntimeUncachedReadObjects returns scoped GVKs that should
// use live reads so legacy unlabeled runtime resources can be adopted instead
// of being hidden by the scoped cache.
func SelfHostedWebhookRuntimeUncachedReadObjects() []client.Object {
	objects := make([]client.Object, 0, len(selfHostedWebhookRuntimeWatchSpecs))
	for _, spec := range selfHostedWebhookRuntimeWatchSpecs {
		if spec.cacheScoped {
			objects = append(objects, spec.newObject())
		}
	}

	return objects
}

func selfHostedWebhookRuntimeCacheSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{
		identityv1.LabelManagedBy: identityv1.ManagedByValue,
		labelAppName:              webhookComponentName,
	})
}

func webhookRuntimeObjectPredicate() predicate.Funcs {
	return metricsRecordingPredicate(metrics.ControllerSelfHostedWebhook, predicateKeepFns{
		Create: func(e event.CreateEvent) bool {
			return isManagedWebhookRuntimeObject(e.Object)
		},
		Update: func(e event.UpdateEvent) bool {
			if !isManagedWebhookRuntimeObject(e.ObjectOld) && !isManagedWebhookRuntimeObject(e.ObjectNew) {
				return false
			}

			return webhookRuntimeUpdateNeedsReconcile(e.ObjectOld, e.ObjectNew)
		},
		Delete: func(e event.DeleteEvent) bool {
			return isManagedWebhookRuntimeObject(e.Object)
		},
		Generic: func(e event.GenericEvent) bool {
			return isManagedWebhookRuntimeObject(e.Object)
		},
	})
}

func webhookRuntimeUpdateNeedsReconcile(oldObj, newObj client.Object) bool {
	oldDeployment, oldOK := oldObj.(*appsv1.Deployment)
	newDeployment, newOK := newObj.(*appsv1.Deployment)

	if !oldOK || !newOK {
		return true
	}

	// Generation covers Spec drift (including Spec.Template.Annotations
	// where the cert fingerprint lives). Top-level annotations are
	// intentionally ignored — the controller stamps none, so observer
	// drift there is not actionable.
	if oldDeployment.Generation != newDeployment.Generation {
		return true
	}

	return !slices.Equal(oldDeployment.Finalizers, newDeployment.Finalizers) ||
		!maps.Equal(oldDeployment.Labels, newDeployment.Labels) ||
		!apiequality.Semantic.DeepEqual(oldDeployment.OwnerReferences, newDeployment.OwnerReferences)
}

func isManagedWebhookRuntimeObject(obj client.Object) bool {
	if obj == nil {
		return false
	}

	labels := obj.GetLabels()
	if labels[identityv1.LabelManagedBy] != identityv1.ManagedByValue {
		return false
	}

	if labels[identityv1.LabelRuntime] == identityv1.RuntimeWebhook {
		return true
	}

	if obj.GetName() == webhookComponentName || obj.GetName() == webhookCASecretName {
		return labels[labelAppName] == webhookComponentName
	}

	return false
}

func ensureRemoteNamespace(ctx context.Context, c client.Client, name string) (controllerutil.OperationResult, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}

	return createOrUpdate(ctx, c, nil, nil, ns, func() error {
		// Pre-existing namespaces we did not create are left alone so we do
		// not adopt user-owned namespaces. controllerutil.CreateOrUpdate
		// runs mutate after Get, so a non-empty ResourceVersion identifies
		// the update path; a no-op mutate then yields OperationResultNone.
		if ns.ResourceVersion != "" && !isManagedWebhookRuntimeObject(ns) {
			return nil
		}

		setWebhookRuntimeLabels(ns)

		return nil
	})
}

func ensureRemoteCASecret(ctx context.Context, c client.Client, namespace string) (controllerutil.OperationResult, []byte, *x509.Certificate, *rsa.PrivateKey, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: namespace}}

	var (
		cert *x509.Certificate
		key  *rsa.PrivateKey
	)

	op, err := createOrUpdate(ctx, c, nil, nil, secret, func() error {
		setWebhookRuntimeLabels(secret)

		if existingCert, existingKey, ok := reusableCASecret(secret); ok {
			cert, key = existingCert, existingKey

			return nil
		}

		certPEM, keyPEM, err := generateWebhookCACertificate()
		if err != nil {
			return err
		}

		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			webhookCACertKey: certPEM,
			webhookCAKeyKey:  keyPEM,
		}

		// Re-parse from the freshly generated PEM so the post-apply path
		// doesn't have to do it again.
		cert, key, _ = reusableCASecret(secret)

		return nil
	})
	if err != nil {
		return op, nil, nil, nil, err
	}

	if cert == nil || key == nil {
		var ok bool

		cert, key, ok = reusableCASecret(secret)
		if !ok {
			return op, nil, nil, nil, fmt.Errorf("remote CA Secret %s/%s is not reusable after apply", namespace, webhookCASecretName)
		}
	}

	return op, secret.Data[webhookCACertKey], cert, key, nil
}

func ensureRemoteServingTLSSecret(ctx context.Context, c client.Client, namespace string, caCert *x509.Certificate, caKey *rsa.PrivateKey) (controllerutil.OperationResult, metav1.Time, string, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}}

	var (
		certNotAfter metav1.Time
		fingerprint  string
	)

	op, err := createOrUpdate(ctx, c, nil, nil, secret, func() error {
		setWebhookRuntimeLabels(secret)

		if notAfter, fp, ok := reusableServingTLSCert(secret, namespace, caCert); ok {
			certNotAfter, fingerprint = notAfter, fp

			return nil
		}

		certPEM, keyPEM, err := generateWebhookServingCertificate(namespace, caCert, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
		if err != nil {
			return err
		}

		secret.Type = corev1.SecretTypeTLS
		secret.Data = map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		}

		return nil
	})
	if err != nil {
		return op, metav1.Time{}, "", err
	}

	if fingerprint == "" {
		var ok bool

		certNotAfter, fingerprint, ok = reusableServingTLSCert(secret, namespace, caCert)
		if !ok {
			return op, metav1.Time{}, "", fmt.Errorf("remote TLS Secret %s/%s is not reusable after apply", namespace, webhookComponentName)
		}
	}

	return op, certNotAfter, fingerprint, nil
}

func reusableCASecret(secret *corev1.Secret) (*x509.Certificate, *rsa.PrivateKey, bool) {
	certPEM := secret.Data[webhookCACertKey]

	keyPEM := secret.Data[webhookCAKeyKey]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, false
	}

	cert, err := parseSingleCertificatePEM(certPEM)
	if err != nil || !cert.IsCA || time.Now().Add(webhookCertRenewalThreshold).After(cert.NotAfter) {
		return nil, nil, false
	}

	key, err := parseRSAPrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, false
	}

	return cert, key, true
}

func reusableServingTLSCert(secret *corev1.Secret, namespace string, caCert *x509.Certificate) (metav1.Time, string, bool) {
	certPEM := secret.Data[corev1.TLSCertKey]

	keyPEM := secret.Data[corev1.TLSPrivateKeyKey]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return metav1.Time{}, "", false
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(tlsCert.Certificate) != 1 {
		return metav1.Time{}, "", false
	}

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil || time.Now().Add(webhookCertRenewalThreshold).After(leaf.NotAfter) {
		return metav1.Time{}, "", false
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)

	commonName, _ := webhookServiceDNSNames(namespace)
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:   commonName,
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return metav1.Time{}, "", false
	}

	return metav1.NewTime(leaf.NotAfter), certificateFingerprint(certPEM), true
}

// webhookServiceDNSNames returns the in-cluster Service common name and
// SANs for the webhook serving cert. CN/DNSName must equal the first SAN.
func webhookServiceDNSNames(namespace string) (commonName string, sans []string) {
	commonName = webhookComponentName + "." + namespace + ".svc"

	return commonName, []string{
		webhookComponentName,
		webhookComponentName + "." + namespace,
		commonName,
		commonName + ".cluster.local",
	}
}

func generateWebhookCACertificate() ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, webhookRSAKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA RSA key: %w", err)
	}

	cert, err := certutil.NewSelfSignedCACert(certutil.Config{
		CommonName: webhookComponentName + "-ca",
		NotBefore:  time.Now().Add(-webhookCertClockSkew),
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}

	return encodeCertificateAndKeyPEM(cert.Raw, key)
}

func generateWebhookServingCertificate(namespace string, caCert *x509.Certificate, caKey *rsa.PrivateKey, extKeyUsages []x509.ExtKeyUsage) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, webhookRSAKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generate RSA key: %w", err)
	}

	serialNumber, err := randomCertificateSerialNumber()
	if err != nil {
		return nil, nil, err
	}

	commonName, sans := webhookServiceDNSNames(namespace)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: commonName,
		},
		DNSNames:              sans,
		NotBefore:             now.Add(-webhookCertClockSkew),
		NotAfter:              now.Add(webhookCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           extKeyUsages,
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create serving certificate: %w", err)
	}

	return encodeCertificateAndKeyPEM(certDER, key)
}

func randomCertificateSerialNumber() (*big.Int, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)

	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial number: %w", err)
	}

	return serialNumber, nil
}

func encodeCertificateAndKeyPEM(certDER []byte, key *rsa.PrivateKey) ([]byte, []byte, error) {
	certPEM := pem.EncodeToMemory(&pem.Block{Type: certutil.CertificateBlockType, Bytes: certDER})

	keyPEM, err := keyutil.MarshalPrivateKeyToPEM(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode private key PEM: %w", err)
	}

	return certPEM, keyPEM, nil
}

func parseSinglePEMBlock(data []byte, expectedType string) (*pem.Block, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != expectedType || len(rest) != 0 {
		return nil, fmt.Errorf("expected a single %s PEM block", expectedType)
	}

	return block, nil
}

func parseSingleCertificatePEM(certPEM []byte) (*x509.Certificate, error) {
	block, err := parseSinglePEMBlock(certPEM, certutil.CertificateBlockType)
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}

	return cert, nil
}

func parseRSAPrivateKeyPEM(keyPEM []byte) (*rsa.PrivateKey, error) {
	block, err := parseSinglePEMBlock(keyPEM, keyutil.RSAPrivateKeyBlockType)
	if err != nil {
		return nil, err
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA private key: %w", err)
	}

	return key, nil
}

func certificateFingerprint(certPEM []byte) string {
	sum := sha256.Sum256(certPEM)

	return hex.EncodeToString(sum[:])
}

func nextWebhookCertRequeue(certNotAfter metav1.Time) time.Duration {
	requeue := time.Until(certNotAfter.Add(-webhookCertRenewalThreshold))
	if requeue < webhookCertRequeueMinimum {
		return webhookCertRequeueMinimum
	}

	if requeue > webhookCertRequeueMaximum {
		return webhookCertRequeueMaximum
	}

	return requeue
}

func ensureRemoteServiceAccount(ctx context.Context, c client.Client, namespace string) (controllerutil.OperationResult, error) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}}

	return createOrUpdate(ctx, c, nil, nil, sa, func() error {
		setWebhookRuntimeLabels(sa)

		return nil
	})
}

func ensureRemoteClusterRole(ctx context.Context, c client.Client, _ string) (controllerutil.OperationResult, error) {
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}

	return createOrUpdate(ctx, c, nil, nil, role, func() error {
		setWebhookRuntimeLabels(role)
		// With --in-cluster=false the pod-identity-webhook does not create CSRs;
		// it only reads ServiceAccount annotations during admission. CSR verbs
		// belong to the --in-cluster=true mode and are intentionally omitted.
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"serviceaccounts"},
				Verbs:     []string{"get", "list", "watch"},
			},
		}

		return nil
	})
}

func ensureRemoteClusterRoleBinding(ctx context.Context, c client.Client, namespace string) (controllerutil.OperationResult, error) {
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}

	return createOrUpdate(ctx, c, nil, nil, binding, func() error {
		setWebhookRuntimeLabels(binding)
		binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: webhookComponentName, Namespace: namespace}}
		binding.RoleRef = rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: webhookComponentName}

		return nil
	})
}

func ensureRemoteService(ctx context.Context, c client.Client, namespace string) (controllerutil.OperationResult, error) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}}

	return createOrUpdate(ctx, c, nil, nil, service, func() error {
		setWebhookRuntimeLabels(service)
		service.Spec.Selector = map[string]string{labelAppName: webhookComponentName}
		// Protocol is set explicitly so API-server-defaulted "TCP" does not
		// produce a spurious diff against an empty desired Protocol on every
		// reconcile, which would trigger no-op Update calls.
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "https",
			Protocol:   corev1.ProtocolTCP,
			Port:       443,
			TargetPort: intstr.FromInt32(443),
		}}

		return nil
	})
}

func ensureRemoteMutatingWebhookConfiguration(ctx context.Context, c client.Client, namespace string, caBundle []byte) (*admissionregistrationv1.MutatingWebhookConfiguration, controllerutil.OperationResult, error) {
	path := "/mutate"
	sideEffects := admissionregistrationv1.SideEffectClassNone
	// failurePolicy=Fail surfaces webhook outages at admission time. With
	// failurePolicy=Ignore, a crashlooping webhook silently passes Pods through
	// without IRSA env injection — workloads run, but every AWS SDK call later
	// fails with AccessDenied far from the root cause.
	failurePolicy := admissionregistrationv1.Fail
	webhook := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}

	op, err := createOrUpdate(ctx, c, nil, nil, webhook, func() error {
		setWebhookRuntimeLabels(webhook)
		webhook.Webhooks = []admissionregistrationv1.MutatingWebhook{{
			Name:          webhookAdmissionName,
			FailurePolicy: &failurePolicy,
			SideEffects:   &sideEffects,
			// amazon-eks-pod-identity-webhook still returns v1beta1-shaped
			// AdmissionReview responses; using v1 causes the API server to
			// reject otherwise successful mutations.
			AdmissionReviewVersions: []string{"v1beta1"},
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				CABundle: caBundle,
				Service: &admissionregistrationv1.ServiceReference{
					Name:      webhookComponentName,
					Namespace: namespace,
					Path:      &path,
				},
			},
			ObjectSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      selfHostedSkipWebhookLabel,
					Operator: metav1.LabelSelectorOpDoesNotExist,
				}},
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
				},
			}},
		}}

		return nil
	})

	return webhook, op, err
}

func ensureRemoteDeployment(ctx context.Context, c client.Client, namespace, image, certFingerprint string) (*appsv1.Deployment, controllerutil.OperationResult, error) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}}
	desiredSelector := webhookDeploymentSelector()

	// Field-by-field assignment (rather than full Spec replacement) preserves
	// API-server-defaulted top-level fields like Strategy / RevisionHistoryLimit
	// / ProgressDeadlineSeconds across reconciles. The container- and podspec-
	// level defaults that would otherwise be wiped by a full Template assignment
	// are set explicitly below for the same reason — without them, every
	// reconcile produces a phantom diff and a no-op Update.
	op, err := createOrUpdate(ctx, c, nil, nil, deployment, func() error {
		setWebhookRuntimeLabels(deployment)

		if deployment.Spec.Selector == nil {
			deployment.Spec.Selector = desiredSelector.DeepCopy()
		} else if !apiequality.Semantic.DeepEqual(deployment.Spec.Selector, desiredSelector) {
			return fmt.Errorf("existing Deployment selector is %v, expected %v; selector is immutable and the Deployment must be recreated", deployment.Spec.Selector, desiredSelector)
		}

		deployment.Spec.Replicas = ptr.To(int32(1))
		deployment.Spec.Template.Labels = mergeWebhookRuntimeLabels(deployment.Spec.Template.Labels, webhookDeploymentPodLabels())
		deployment.Spec.Template.Annotations = mergeStringMap(deployment.Spec.Template.Annotations, map[string]string{webhookServingCertFingerprintKey: certFingerprint})
		deployment.Spec.Template.Spec.ServiceAccountName = webhookComponentName
		deployment.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
		deployment.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
		deployment.Spec.Template.Spec.SchedulerName = corev1.DefaultSchedulerName
		deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(30))
		ensureWebhookPodSecurityContext(&deployment.Spec.Template.Spec)

		// The named webhook container and cert volume are operator-owned because
		// they encode the runtime contract. Other containers, volumes, metadata,
		// and Pod scheduling/tuning fields are intentionally preserved.
		deployment.Spec.Template.Spec.Containers = upsertNamed(deployment.Spec.Template.Spec.Containers, desiredWebhookContainer(namespace, image), func(c corev1.Container) string { return c.Name })
		deployment.Spec.Template.Spec.Volumes = upsertNamed(deployment.Spec.Template.Spec.Volumes, desiredWebhookCertVolume(), func(v corev1.Volume) string { return v.Name })

		return nil
	})

	return deployment, op, err
}

func webhookDeploymentSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{labelAppName: webhookComponentName}}
}

func webhookDeploymentPodLabels() map[string]string {
	labels := webhookRuntimeLabels()
	// The selfHostedSkipWebhookLabel keeps the webhook from injecting itself on
	// its own Pods; it lives only in the PodTemplate, not on the Deployment
	// metadata, so it is not part of webhookRuntimeLabels.
	labels[selfHostedSkipWebhookLabel] = webhookSkipLabelValue

	return labels
}

func ensureWebhookPodSecurityContext(podSpec *corev1.PodSpec) {
	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}

	podSpec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
}

func desiredWebhookContainer(namespace, image string) corev1.Container {
	return corev1.Container{
		Name:    "webhook",
		Image:   image,
		Command: []string{"/webhook"},
		Args: []string{
			"--in-cluster=false",
			"--namespace=" + namespace,
			"--service-name=" + webhookComponentName,
			"--annotation-prefix=" + selfHostedAnnotationPrefix,
			"--token-audience=" + remoteirsa.STSAudience,
		},
		Ports: []corev1.ContainerPort{{
			Name:          "https",
			Protocol:      corev1.ProtocolTCP,
			ContainerPort: 443,
		}},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "cert",
			MountPath: "/etc/webhook/certs",
			ReadOnly:  true,
		}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		LivenessProbe:            webhookHealthProbe(10, 20),
		ReadinessProbe:           webhookHealthProbe(5, 10),
		ImagePullPolicy:          corev1.PullIfNotPresent,
		SecurityContext:          desiredWebhookContainerSecurityContext(),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
}

// webhookHealthProbe returns an HTTPS probe against the upstream
// pod-identity-webhook /healthz endpoint on the main TLS port. kubelet skips
// certificate verification for probe traffic, so the self-signed serving cert
// suffices.
func webhookHealthProbe(initialDelay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/healthz",
				Port:   intstr.FromString("https"),
				Scheme: corev1.URISchemeHTTPS,
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      5,
		FailureThreshold:    3,
		SuccessThreshold:    1,
	}
}

func desiredWebhookContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"NET_BIND_SERVICE"},
		},
		RunAsGroup:     ptr.To(webhookRuntimeUserID),
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To(webhookRuntimeUserID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func desiredWebhookCertVolume() corev1.Volume {
	return corev1.Volume{
		Name: "cert",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: webhookComponentName},
		},
	}
}

func upsertNamed[T any](items []T, desired T, name func(T) string) []T {
	target := name(desired)
	for i := range items {
		if name(items[i]) == target {
			items[i] = desired

			return items
		}
	}

	return append(items, desired)
}

func webhookRuntimeLabels() map[string]string {
	return map[string]string{
		labelAppName:              webhookComponentName,
		identityv1.LabelManagedBy: identityv1.ManagedByValue,
		identityv1.LabelDelivery:  string(identityv1.DeliveryTypeSelfHostedIRSA),
		identityv1.LabelRuntime:   identityv1.RuntimeWebhook,
	}
}

func setWebhookRuntimeLabels(obj client.Object) {
	obj.SetLabels(mergeWebhookRuntimeLabels(obj.GetLabels(), webhookRuntimeLabels()))
}

func mergeWebhookRuntimeLabels(dst, src map[string]string) map[string]string {
	labels := mergeStringMap(dst, src)
	delete(labels, identityv1.LabelConfigUID)
	delete(labels, identityv1.LabelInventoryNS)

	return labels
}

func remoteMutatingWebhookConfigurationExists(ctx context.Context, c client.Client) (bool, error) {
	webhook := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}

	err := c.Get(ctx, client.ObjectKeyFromObject(webhook), webhook)
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("get remote MutatingWebhookConfiguration %q: %w", webhookComponentName, err)
	}

	return true, nil
}

func assessRemoteDeploymentReadiness(deployment *appsv1.Deployment) webhookDeploymentReadiness {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}

	// Stale status: Deployment controller has not yet observed the current Spec,
	// so the rollout fields below refer to the previous revision. Trusting them
	// could rotate caBundle against old Pods still serving the previous TLS cert.
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return webhookDeploymentReadiness{
			Reason: identityv1.ReasonWebhookDeploymentObservedGenerationLag,
			Message: fmt.Sprintf(
				"remote webhook Deployment status is stale: observedGeneration=%d, generation=%d",
				deployment.Status.ObservedGeneration, deployment.Generation,
			),
		}
	}

	// Rollout in progress: AvailableReplicas can still count old-revision Pods,
	// so wait for UpdatedReplicas to reach desired before trusting availability.
	if deployment.Status.UpdatedReplicas < desired {
		return webhookDeploymentReadiness{
			Reason: identityv1.ReasonWebhookDeploymentRolloutInProgress,
			Message: fmt.Sprintf(
				"remote webhook Deployment rollout in progress: updatedReplicas=%d, desired=%d",
				deployment.Status.UpdatedReplicas, desired,
			),
		}
	}

	if deployment.Status.AvailableReplicas < desired {
		return webhookDeploymentReadiness{
			Reason: identityv1.ReasonWebhookDeploymentReplicasUnavailable,
			Message: fmt.Sprintf(
				"remote webhook Deployment has insufficient available replicas: availableReplicas=%d, desired=%d",
				deployment.Status.AvailableReplicas, desired,
			),
		}
	}

	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return webhookDeploymentReadiness{Ready: true}
		}
	}

	return webhookDeploymentReadiness{
		Reason:  identityv1.ReasonWaitingForWebhookDeployment,
		Message: "remote webhook Deployment Available condition is not True",
	}
}

func mutatingWebhookConfigurationSynced(webhook *admissionregistrationv1.MutatingWebhookConfiguration, namespace string, caBundle []byte) bool {
	if len(webhook.Webhooks) != 1 {
		return false
	}

	clientConfig := webhook.Webhooks[0].ClientConfig
	if clientConfig.Service == nil {
		return false
	}

	return clientConfig.Service.Name == webhookComponentName &&
		clientConfig.Service.Namespace == namespace &&
		bytes.Equal(clientConfig.CABundle, caBundle)
}

func deleteRemoteWebhookRuntime(ctx context.Context, c client.Client, namespace string) error {
	if namespace == "" {
		namespace = identityv1.DefaultSelfHostedWebhookNamespace
	}

	// MWHC must be deleted first so the API server stops routing admission
	// requests before the backing Deployment, Service, and Secrets disappear;
	// parallelizing this with the rest would race the Pod termination against
	// in-flight admissions.
	mwhc := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}}
	if err := deleteManagedRemoteWebhookRuntimeObject(ctx, c, mwhc); err != nil {
		return err
	}

	parallel := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: webhookCASecretName, Namespace: namespace}},
		// Role/RoleBinding stay in cleanup for legacy garbage collection of pre-RBAC-04 empty objects.
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName, Namespace: namespace}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: webhookComponentName}},
	}

	g, gCtx := errgroup.WithContext(ctx)
	for _, obj := range parallel {
		g.Go(func() error {
			return deleteManagedRemoteWebhookRuntimeObject(gCtx, c, obj)
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("delete remote webhook runtime: %w", err)
	}

	return nil
}

// errUnmanagedRemoteRuntime sentinel signals that a remote webhook runtime
// object exists but lacks the operator's managed-by label and cannot be
// safely deleted. Callers compare via errors.Is to surface an accurate
// DeletionBlocked Condition reason.
var errUnmanagedRemoteRuntime = errors.New("remote webhook runtime object is not managed by this operator")

// isErrUnmanagedRemoteRuntime reports whether err originated from the
// remote webhook runtime cleanup path refusing to delete an unmanaged
// cluster-scoped singleton. Used by reconcileDelete to pick the
// RemoteRuntimeUnmanaged Condition reason for accurate observability.
func isErrUnmanagedRemoteRuntime(err error) bool {
	return errors.Is(err, errUnmanagedRemoteRuntime)
}

func deleteManagedRemoteWebhookRuntimeObject(ctx context.Context, c client.Client, obj client.Object) error {
	log := logf.FromContext(ctx)

	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("get remote webhook runtime object %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}

	if !isManagedWebhookRuntimeObject(obj) {
		if obj.GetNamespace() == "" {
			// Deterministic cluster-scoped singleton (MWHC/ClusterRole/ClusterRoleBinding,
			// all named "pod-identity-webhook"). Refusing to delete here keeps the
			// deletion blocked via the caller chain unless the operator sets the
			// force-delete annotation on the AWSWorkloadIdentityConfig.
			log.Info("refusing to delete unlabeled cluster-scoped webhook runtime object",
				"kind", fmt.Sprintf("%T", obj), "name", obj.GetName())

			return fmt.Errorf("%w: refusing to delete cluster-scoped %T %q: lacks %s=%s label; set %s annotation on the AWSWorkloadIdentityConfig to force deletion and leave this object behind",
				errUnmanagedRemoteRuntime, obj, obj.GetName(), identityv1.LabelManagedBy, identityv1.ManagedByValue, identityv1.ForceDeleteAnnotation)
		}

		log.V(1).Info("skipping deletion of unlabeled namespaced webhook runtime object",
			"namespace", obj.GetNamespace(), "name", obj.GetName(), "kind", fmt.Sprintf("%T", obj))

		return nil
	}

	if err := client.IgnoreNotFound(c.Delete(ctx, obj)); err != nil {
		return fmt.Errorf("delete remote webhook runtime object %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
}
