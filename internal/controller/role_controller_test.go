package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	eksv1alpha1 "github.com/aws-controllers-k8s/eks-controller/apis/v1alpha1"
	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

// testInlinePolicyDocument is the canonical valid IAM policy object used to drive
// reconcileGeneratedPolicy through its upsert path in tests.
func testInlinePolicyDocument() *runtime.RawExtension {
	return &runtime.RawExtension{Raw: []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"*"}]}`)}
}

func TestRoleReconcileAddsFinalizerWithoutExplicitRequeue(t *testing.T) {
	role := testAWSServiceAccountRole()
	localClient := testConfigClient(t, role)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Recorder: recorder,
	}

	assertFinalizerAddedOnFirstReconcile(t, localClient, reconciler, role, &identityv1.AWSServiceAccountRole{}, identityv1.ServiceAccountRoleFinalizer, recorder)
}

func TestRoleResultWithAnnotationDeliverySafetyRequeue(t *testing.T) {
	for _, delivery := range []identityv1.DeliveryType{
		identityv1.DeliveryTypeSelfHostedIRSA,
		identityv1.DeliveryTypeEKSIRSA,
	} {
		role := &identityv1.AWSServiceAccountRole{}
		setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

		result := roleResultWithAnnotationDeliverySafetyRequeue(delivery, role, ctrl.Result{})
		if result.RequeueAfter != selfHostedSteadyStateRequeue {
			t.Fatalf("expected annotation delivery safety requeue %s for %s, got %s", selfHostedSteadyStateRequeue, delivery, result.RequeueAfter)
		}
	}
}

func TestRoleResultWithAnnotationDeliverySafetyRequeueSkipsNonAnnotationDeliveryOrExplicitResult(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{}
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	eksResult := roleResultWithAnnotationDeliverySafetyRequeue(identityv1.DeliveryTypeEKSPodIdentity, role, ctrl.Result{})
	if eksResult.RequeueAfter != dependencySteadyStateRequeue {
		t.Fatalf("expected dependency safety requeue %s, got %s", dependencySteadyStateRequeue, eksResult.RequeueAfter)
	}

	explicit := ctrl.Result{RequeueAfter: 30 * time.Second}

	selfHostedResult := roleResultWithAnnotationDeliverySafetyRequeue(identityv1.DeliveryTypeSelfHostedIRSA, role, explicit)
	if selfHostedResult != explicit {
		t.Fatalf("expected explicit requeue result to be preserved, got %#v", selfHostedResult)
	}
}

func TestComputeRoleReadyStateRequiresConfigReadyForSelfHosted(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{}
	config := &identityv1.AWSWorkloadIdentityConfig{ObjectMeta: metav1.ObjectMeta{Name: identityv1.DefaultName, Namespace: testInventoryNamespace}}

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionRoleReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonWaitingForWebhookDeployment, "waiting")

	status, reason, _ := computeRoleReadyState(role, identityv1.DeliveryTypeSelfHostedIRSA, config)
	if status != metav1.ConditionFalse || reason != identityv1.ReasonConfigNotReady {
		t.Fatalf("expected ConfigNotReady, got status=%s reason=%s", status, reason)
	}
}

func TestRoleReconcileNormalOperatorConfigUnavailablePreservesACKResources(t *testing.T) {
	role := testAWSServiceAccountRole()
	ackResources := sentinelACKResources()
	role.Status.ACKResources = ackResources
	localClient := testConfigClient(t, role)
	reconciler := &AWSServiceAccountRoleReconciler{Client: localClient}

	result, err := reconciler.reconcileNormal(context.Background(), role)
	if err != nil {
		t.Fatalf("expected operator config unavailability to patch status without error, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue, got %#v", result)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), types.NamespacedName{Namespace: role.Namespace, Name: role.Name}, stored); err != nil {
		t.Fatal(err)
	}

	assertACKResources(t, stored.Status.ACKResources, ackResources)
}

func TestRoleReconcileNormalManagedPoliciesOnlyClearsGeneratedPolicyACKResource(t *testing.T) {
	role := testAWSServiceAccountRole()
	role.Status.GeneratedPolicyARN = "arn:aws:iam::123456789012:policy/stale"
	role.Status.ACKResources = []identityv1.ACKResourceStatus{{
		APIVersion: "iam.services.k8s.aws/v1alpha1",
		Kind:       ackChildKindPolicy,
		Namespace:  role.Namespace,
		Name:       identityaws.PolicyName(role),
	}}
	existingPolicy := &iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace}}
	// The ownership-aware prune in reconcileGeneratedPolicy only deletes the
	// generated Policy when it carries this role's controllerRef stamp. Stamp
	// the controllerRef the same way createOrUpdate does so this test still
	// exercises the "we own it, clear it" semantics.
	stampRoleControllerRef(role, existingPolicy, role.UID)

	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	localClient := testConfigClient(t, role, testOperatorConfig(), config, testResolvedClusterProfile(role.Namespace), existingPolicy)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if result, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected managed-policy-only reconcile to succeed, got result=%#v err=%v", result, err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), types.NamespacedName{Namespace: role.Namespace, Name: role.Name}, stored); err != nil {
		t.Fatal(err)
	}

	if stored.Status.GeneratedPolicyARN != "" {
		t.Fatalf("expected stale generated policy ARN to be cleared, got %q", stored.Status.GeneratedPolicyARN)
	}

	if len(stored.Status.ACKResources) != 1 || stored.Status.ACKResources[0].Kind != ackChildKindRole {
		t.Fatalf("expected only IAM Role ACKResource after generated policy removal, got %#v", stored.Status.ACKResources)
	}

	storedPolicy := &iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace}}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(storedPolicy), storedPolicy); !apierrors.IsNotFound(err) {
		t.Fatalf("expected generated IAM Policy ACK CR to be deleted, got %v", err)
	}
}

// TestRoleReconcileNormalManagedPoliciesOnlySkipsForeignOwnedGeneratedPolicy is
// the regression test for the steady-state prune branch in
// reconcileGeneratedPolicy: a Policy sharing the generated name but whose
// controllerRef points at a different role UID must NOT be collateral-deleted
// when this role clears spec.PolicyDocument. The role's own status must still
// converge to "managed policies only" so observability stays consistent.
func TestRoleReconcileNormalManagedPoliciesOnlySkipsForeignOwnedGeneratedPolicy(t *testing.T) {
	role := testAWSServiceAccountRole()
	role.Status.GeneratedPolicyARN = "arn:aws:iam::123456789012:policy/stale"
	role.Status.ACKResources = []identityv1.ACKResourceStatus{{
		APIVersion: "iam.services.k8s.aws/v1alpha1",
		Kind:       ackChildKindPolicy,
		Namespace:  role.Namespace,
		Name:       identityaws.PolicyName(role),
	}}

	foreignUID := types.UID("some-other-role-uid")
	foreignPolicy := &iamv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace}}
	stampRoleControllerRef(role, foreignPolicy, foreignUID)

	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	localClient := testConfigClient(t, role, testOperatorConfig(), config, testResolvedClusterProfile(role.Namespace), foreignPolicy)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if result, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected managed-policy-only reconcile to succeed, got result=%#v err=%v", result, err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), types.NamespacedName{Namespace: role.Namespace, Name: role.Name}, stored); err != nil {
		t.Fatal(err)
	}

	if stored.Status.GeneratedPolicyARN != "" {
		t.Fatalf("expected stale generated policy ARN to be cleared even when prune is skipped, got %q", stored.Status.GeneratedPolicyARN)
	}

	if findACKResourceByKindName(stored.Status.ACKResources, ackChildKindPolicy, identityaws.PolicyName(role)) != nil {
		t.Fatalf("expected Policy ACKResource entry to be removed from status even when prune is skipped, got %#v", stored.Status.ACKResources)
	}

	survivingPolicy := &iamv1alpha1.Policy{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(foreignPolicy), survivingPolicy); err != nil {
		t.Fatalf("expected foreign-owned IAM Policy %s/%s to remain after prune skip, got %v", foreignPolicy.Namespace, foreignPolicy.Name, err)
	}

	if owner := metav1.GetControllerOf(survivingPolicy); owner == nil || owner.UID != foreignUID {
		t.Fatalf("expected foreign controllerRef UID %q to remain on surviving Policy, got %#v", foreignUID, survivingPolicy.OwnerReferences)
	}
}

func TestRoleReconcileNormalPersistsStatusOnIAMRoleApplyError(t *testing.T) {
	role := testAWSServiceAccountRole()
	role.Generation = 3
	setCondition(&role.Status.Conditions, role.Generation-1, identityv1.ConditionRoleReady, metav1.ConditionTrue, identityv1.ReasonReady, "previously ready")

	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	operatorConfig := testOperatorConfig()
	clusterProfile := testResolvedClusterProfile(role.Namespace)
	iamRoleApplyErr := errors.New("simulated ACK Role apply failure")
	localClient := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role, operatorConfig, config, clusterProfile).
		WithStatusSubresource(role, operatorConfig, config, clusterProfile).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*iamv1alpha1.Role); ok {
					return iamRoleApplyErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if _, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); !errors.Is(err, iamRoleApplyErr) {
		t.Fatalf("expected IAM Role apply failure to surface, got %v", err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	if stored.Status.ObservedGeneration != role.Generation {
		t.Fatalf("expected ObservedGeneration=%d to be persisted before IAM Role error, got %d", role.Generation, stored.Status.ObservedGeneration)
	}

	for _, tc := range []struct{ condType, reason string }{
		{identityv1.ConditionOperatorConfigReady, identityv1.ReasonReady},
		{identityv1.ConditionConfigResolved, identityv1.ReasonResolved},
		{identityv1.ConditionTrustPolicyReady, identityv1.ReasonRendered},
	} {
		assertCondition(t, stored.Status.Conditions, tc.condType, metav1.ConditionTrue, tc.reason)
	}

	// The pre-set ConditionRoleReady=True from the previous generation must be
	// degraded to False/ChildApplyFailed because the current generation observed
	// the IAM Role apply error. Leaving the stale True would let callers see a
	// hub Ready signal that no longer reflects the live reconcile outcome.
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionRoleReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
}

// TestRoleReconcileNormalIAMRoleApplyErrorPreservesACKResources is the
// regression test: previously reconcileNormal reset
// role.Status.ACKResources to an empty slice before any per-resource upsert,
// so a transient IAM Role apply error wiped out previously observed ACK
// metadata for both the generated Policy and the IAM Role. The fix replaces
// the wholesale reset with kind-keyed upsert + targeted prune, which means
// observed-but-not-yet-rebuilt entries must survive a downstream apply error.
func TestRoleReconcileNormalIAMRoleApplyErrorPreservesACKResources(t *testing.T) {
	role := testAWSServiceAccountRole()
	role.Generation = 4
	// Set a customer-managed PolicyDocument so reconcileGeneratedPolicy goes
	// through the upsert path (rather than the managed-policies-only branch
	// that legitimately removes the Policy entry).
	role.Spec.PolicyDocument = testInlinePolicyDocument()

	preExistingPolicy := preExistingPolicyACKResourceStatus(role)
	preExistingRole := preExistingRoleACKResourceStatus(role)
	role.Status.ACKResources = []identityv1.ACKResourceStatus{preExistingPolicy, preExistingRole}

	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	iamRoleApplyErr := errors.New("simulated ACK Role apply failure")
	localClient := newRoleControllerFakeClientFailingOnIAMRoleCreate(t, role, config, iamRoleApplyErr)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if _, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); !errors.Is(err, iamRoleApplyErr) {
		t.Fatalf("expected IAM Role apply failure to surface, got %v", err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	// The pre-existing Role entry must survive the IAM Role apply failure: the
	// reset that motivated this regression test would have overwritten it with an empty
	// slice before any successful per-resource upsert.
	if findACKResourceByKindName(stored.Status.ACKResources, ackChildKindRole, identityaws.RoleName(role)) == nil {
		t.Fatalf("expected pre-existing IAM Role ACKResource entry to be preserved after IAM Role apply error, got %#v", stored.Status.ACKResources)
	}

	// The Policy reconcile completed successfully before the IAM Role apply
	// failed, so the Policy entry must still be present (upsert may have
	// replaced it in place since the (Kind, Name) tuple is identical).
	policyEntry := findACKResourceByKindName(stored.Status.ACKResources, ackChildKindPolicy, identityaws.PolicyName(role))
	if policyEntry == nil {
		t.Fatalf("expected IAM Policy ACKResource entry to remain after IAM Role apply error, got %#v", stored.Status.ACKResources)
	}

	if policyEntry.Namespace != role.Namespace {
		t.Fatalf("expected Policy ACKResource entry namespace %q, got %q", role.Namespace, policyEntry.Namespace)
	}
}

// TestReconcileGeneratedPolicyHoldsPolicyReadyUntilACKARNObserved is the
// regression test for the ARN-observation race in reconcileGeneratedPolicy:
// when ACK reports the IAM Policy as ResourceSynced=True but has not yet
// late-initialized Status.ACKResourceMetadata.ARN, the controller MUST keep
// ConditionPolicyReady=False with ReasonACKResourceIdentifierPending and MUST
// NOT publish an empty Status.GeneratedPolicyARN as if the identifier had been
// observed. Without this gate the aggregate hub Ready condition could flip True
// before downstream IAM Role attachment could discover a non-empty policy ARN.
func TestReconcileGeneratedPolicyHoldsPolicyReadyUntilACKARNObserved(t *testing.T) {
	role := testAWSServiceAccountRole()
	role.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}
	// Customer-managed PolicyDocument forces reconcileGeneratedPolicy through
	// the upsert path so we exercise the synced-but-no-ARN late-init window.
	role.Spec.PolicyDocument = testInlinePolicyDocument()

	// Pre-create the ACK Policy with ResourceSynced=True but ARN unset; the
	// controllerRef stamp lets the createOrUpdate path treat it as already owned
	// so we stay on the steady-state branch instead of the create branch.
	syncedPolicyWithoutARN := &iamv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace},
		Status: iamv1alpha1.PolicyStatus{
			ACKResourceMetadata: &ackv1alpha1.ResourceMetadata{ARN: nil},
			Conditions: []*ackv1alpha1.Condition{{
				Type:   ackv1alpha1.ConditionTypeResourceSynced,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	stampRoleControllerRef(role, syncedPolicyWithoutARN, role.UID)

	config := testSelfHostedConfig()
	// EKSPodIdentity keeps the test focused on the Policy condition and avoids
	// remote-cluster paths.
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	localClient := testConfigClient(t, role, testOperatorConfig(), config, testResolvedClusterProfile(role.Namespace), syncedPolicyWithoutARN)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if _, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected reconcileNormal to persist status without error while waiting on Policy ARN, got %v", err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	if stored.Status.GeneratedPolicyARN != "" {
		t.Fatalf("expected GeneratedPolicyARN to remain empty until ACK observes the ARN, got %q", stored.Status.GeneratedPolicyARN)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionPolicyReady, metav1.ConditionFalse, identityv1.ReasonACKResourceIdentifierPending)

	// The aggregate hub Ready condition must also stay gated: even though ACK
	// reported the Policy as ResourceSynced=True, the missing ARN means Ready
	// must not flip True via any other code path.
	if meta.IsStatusConditionTrue(stored.Status.Conditions, identityv1.ConditionReady) {
		t.Fatalf("expected aggregate Ready to remain not-True while Policy ARN is pending, got %#v", meta.FindStatusCondition(stored.Status.Conditions, identityv1.ConditionReady))
	}
}

// preExistingPolicyACKResourceStatus returns the ACKResource entry that a
// previous reconcile would have recorded for the controller-managed IAM
// Policy CR.
func preExistingPolicyACKResourceStatus(role *identityv1.AWSServiceAccountRole) identityv1.ACKResourceStatus {
	return identityv1.ACKResourceStatus{
		APIVersion: iamv1alpha1.GroupVersion.String(),
		Kind:       ackChildKindPolicy,
		Namespace:  role.Namespace,
		Name:       identityaws.PolicyName(role),
	}
}

// preExistingRoleACKResourceStatus returns the ACKResource entry that a
// previous reconcile would have recorded for the IAM Role CR.
func preExistingRoleACKResourceStatus(role *identityv1.AWSServiceAccountRole) identityv1.ACKResourceStatus {
	return identityv1.ACKResourceStatus{
		APIVersion: iamv1alpha1.GroupVersion.String(),
		Kind:       ackChildKindRole,
		Namespace:  role.Namespace,
		Name:       identityaws.RoleName(role),
	}
}

// newRoleControllerFakeClientFailingOnIAMRoleCreate builds the fake client
// matrix used by the IAM-Role-apply-error regression test. Extracted to keep
// the test body short enough for funlen.
func newRoleControllerFakeClientFailingOnIAMRoleCreate(t *testing.T, role *identityv1.AWSServiceAccountRole, config *identityv1.AWSWorkloadIdentityConfig, iamRoleApplyErr error) client.WithWatch {
	t.Helper()

	operatorConfig := testOperatorConfig()
	clusterProfile := testResolvedClusterProfile(role.Namespace)

	return fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role, operatorConfig, config, clusterProfile).
		WithStatusSubresource(role, operatorConfig, config, clusterProfile).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*iamv1alpha1.Role); ok {
					return iamRoleApplyErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
}

// TestRoleReconcileNormalIAMPolicyApplyErrorSetsPolicyReadyFalse is the
// regression test for the Policy-apply-error branch of reconcileGeneratedPolicy:
// when createOrUpdate fails for the controller-managed IAM Policy CR, the
// reconcile must publish ConditionPolicyReady=False/ChildApplyFailed alongside
// the lockstep DeliveryReady/Ready degrade so a stale True from a previous
// generation cannot mask the live failure on the hub.
func TestRoleReconcileNormalIAMPolicyApplyErrorSetsPolicyReadyFalse(t *testing.T) {
	role := testAWSServiceAccountRole()
	// Customer-managed PolicyDocument forces reconcileGeneratedPolicy through
	// the createOrUpdate path so the interceptor can fail on the Policy Create.
	role.Spec.PolicyDocument = testInlinePolicyDocument()

	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	operatorConfig := testOperatorConfig()
	clusterProfile := testResolvedClusterProfile(role.Namespace)
	policyApplyErr := errors.New("simulated ACK Policy apply failure")
	localClient := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role, operatorConfig, config, clusterProfile).
		WithStatusSubresource(role, operatorConfig, config, clusterProfile).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*iamv1alpha1.Policy); ok {
					return policyApplyErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if _, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); !errors.Is(err, policyApplyErr) {
		t.Fatalf("expected IAM Policy apply failure to surface, got %v", err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionPolicyReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
}

// TestRoleReconcileNormalOversizedPolicyDocumentSetsPolicyReadyFalse is the
// regression test for the PolicyDocumentString canonicalize-error branch of
// reconcileGeneratedPolicy: an AWS-oversized JSON payload must publish
// ConditionPolicyReady=False/InvalidSpec alongside the lockstep
// DeliveryReady/Ready degrade so authors of broken spec see the failure on
// status instead of silently keeping a stale True. This test calls
// reconcileGeneratedPolicy directly to keep assertions on the in-memory
// status untouched by any client serialization side-effect.
func TestRoleReconcileNormalOversizedPolicyDocumentSetsPolicyReadyFalse(t *testing.T) {
	role := testAWSServiceAccountRole()
	role.Spec.PolicyDocument = &runtime.RawExtension{Raw: []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"` +
		strings.Repeat("a", 6144) +
		`"}]}`)}

	reconciler := &AWSServiceAccountRoleReconciler{
		Client: fake.NewClientBuilder().WithScheme(testControllerScheme(t)).Build(),
		Scheme: testControllerScheme(t),
	}

	_, err := reconciler.reconcileGeneratedPolicy(logr.NewContext(context.Background(), logr.Discard()), logr.Discard(), role, identityv1.DeliveryTypeEKSPodIdentity, role.Spec.PolicyARNs)
	if err == nil || !strings.Contains(err.Error(), "canonicalize policy document") {
		t.Fatalf("expected canonicalize policy document error to surface, got %v", err)
	}

	assertCondition(t, role.Status.Conditions, identityv1.ConditionPolicyReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
	assertCondition(t, role.Status.Conditions, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
	assertCondition(t, role.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
}

// TestRoleReconcileNormalPodIdentityAssociationApplyErrorSetsPodIdentityAssocReadyFalse
// is the regression test for the PodIdentityAssociation-apply-error branch of
// reconcilePodIdentityAssociation: when createOrUpdate fails for the
// controller-managed PodIdentityAssociation CR, the reconcile must publish
// ConditionPodIdentityAssocReady=False/ChildApplyFailed alongside the lockstep
// DeliveryReady/Ready degrade. Reaching this branch requires the upstream IAM
// Role reconcile to have already succeeded (so role.Status.RoleARN is set),
// which is why this test pre-creates an IAM Role CR with an observed ARN.
func TestRoleReconcileNormalPodIdentityAssociationApplyErrorSetsPodIdentityAssocReadyFalse(t *testing.T) {
	role := testAWSServiceAccountRole()
	// EKSPodIdentity delivery is required to enter reconcilePodIdentityAssociation.
	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	// Pre-create the IAM Role with ARN so reconcileIAMRole observes a non-empty
	// RoleARN, which is the gate that lets reconcilePodIdentityAssociation run
	// past its early return.
	resourceARN := ackv1alpha1.AWSResourceName(testRoleARN)
	iamRole := &iamv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace},
		Status: iamv1alpha1.RoleStatus{
			ACKResourceMetadata: &ackv1alpha1.ResourceMetadata{ARN: &resourceARN},
			Conditions: []*ackv1alpha1.Condition{{
				Type:   ackv1alpha1.ConditionTypeResourceSynced,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	operatorConfig := testOperatorConfig()
	clusterProfile := testResolvedClusterProfile(role.Namespace)
	piaApplyErr := errors.New("simulated ACK PodIdentityAssociation apply failure")
	localClient := fake.NewClientBuilder().
		WithScheme(testControllerScheme(t)).
		WithObjects(role, operatorConfig, config, clusterProfile, iamRole).
		WithStatusSubresource(role, operatorConfig, config, clusterProfile, iamRole).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByServiceAccount, IndexAWSServiceAccountRoleBySA).
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetOwnerRef, IndexAWSServiceAccountRoleByReplicaSetOwnerRef).
		WithIndex(&identityv1.AWSWorkloadIdentityConfig{}, IndexConfigByResolvedCluster, IndexAWSWorkloadIdentityConfigByResolvedCluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*eksv1alpha1.PodIdentityAssociation); ok {
					return piaApplyErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Scheme:   testControllerScheme(t),
		Resolver: inventory.Resolver{Client: localClient},
	}

	if _, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); !errors.Is(err, piaApplyErr) {
		t.Fatalf("expected PodIdentityAssociation apply failure to surface, got %v", err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionPodIdentityAssocReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
	assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonChildApplyFailed)
}

// TestRoleReconcileNormalSelfHostedPrunesStalePodIdentityAssociationACKResource
// covers the prune logic that drops PodIdentityAssociation entries when the
// active delivery type is no longer EKSPodIdentity. Without the prune, a
// transition from EKSPodIdentity -> SelfHostedIRSA would leak an orphaned
// PodIdentityAssociation status entry.
func TestRoleReconcileNormalSelfHostedPrunesStalePodIdentityAssociationACKResource(t *testing.T) {
	role := testAWSServiceAccountRole()
	stalePIA := identityv1.ACKResourceStatus{
		APIVersion: "eks.services.k8s.aws/v1alpha1",
		Kind:       ackChildKindPodIdentityAssociation,
		Namespace:  role.Namespace,
		Name:       identityaws.PodIdentityAssociationName(role),
	}
	role.Status.ACKResources = []identityv1.ACKResourceStatus{stalePIA}

	config := testRoleReadySelfHostedConfig()
	iamRole := testIAMRoleWithARN(role, testRoleARN)
	localClient := testConfigClient(t, role, testOperatorConfig(), config, testResolvedClusterProfile(role.Namespace), iamRole)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		Scheme:    testControllerScheme(t),
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
		Resolver:  inventory.Resolver{Client: localClient},
	}

	if _, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected self-hosted reconcile to persist status without error, got %v", err)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	if findACKResourceByKindName(stored.Status.ACKResources, ackChildKindPodIdentityAssociation, identityaws.PodIdentityAssociationName(role)) != nil {
		t.Fatalf("expected stale PodIdentityAssociation ACKResource entry to be pruned for self-hosted delivery, got %#v", stored.Status.ACKResources)
	}
}

// findACKResourceByKindName returns the entry matching the (kind, name) tuple
// or nil if no such entry exists. It exists to keep regression-test
// assertions readable when reconcile may legitimately reorder or replace
// entries via upsert.
func findACKResourceByKindName(entries []identityv1.ACKResourceStatus, kind, name string) *identityv1.ACKResourceStatus {
	for i := range entries {
		if entries[i].Kind == kind && entries[i].Name == name {
			return &entries[i]
		}
	}

	return nil
}

func TestRoleReconcileNormalPersistsDeliveryContextBeforeRemoteAnnotationPatch(t *testing.T) {
	role := testAWSServiceAccountRole()
	config := testRoleReadySelfHostedConfig()
	iamRole := testIAMRoleWithARN(role, testRoleARN)
	remoteServiceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	remoteClient := fakeClient(t, remoteServiceAccount)
	localClient := testConfigClient(t, role, testOperatorConfig(), config, testResolvedClusterProfile(role.Namespace), iamRole)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		Scheme:    testControllerScheme(t),
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: remoteClient}},
		Resolver:  inventory.Resolver{Client: localClient},
	}

	result, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role)
	if err != nil {
		t.Fatalf("expected first self-hosted reconcile to persist status before remote patch, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue after persisting delivery context, got %#v", result)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	if stored.Status.RoleARN != testRoleARN ||
		stored.Status.DeliveryType != identityv1.DeliveryTypeSelfHostedIRSA ||
		stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected persisted deletion context, got status=%#v", stored.Status)
	}

	storedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(storedSA), storedSA); err != nil {
		t.Fatal(err)
	}

	if storedSA.Annotations[selfHostedRoleARNAnnotation] != "" {
		t.Fatalf("expected first pass not to patch remote annotations before status persistence, got %#v", storedSA.Annotations)
	}
}

func TestRoleReconcileNormalEKSIRSAPersistsDeliveryContextBeforeRemoteAnnotationPatch(t *testing.T) {
	role := testAWSServiceAccountRole()
	config := testRoleReadyEKSIRSAConfig()
	iamRole := testIAMRoleWithARN(role, testRoleARN)
	remoteServiceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	remoteClient := fakeClient(t, remoteServiceAccount)
	localClient := testConfigClient(t, role, testOperatorConfig(), config, testResolvedClusterProfile(role.Namespace), iamRole)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		Scheme:    testControllerScheme(t),
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: remoteClient}},
		Resolver:  inventory.Resolver{Client: localClient},
	}

	result, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role)
	if err != nil {
		t.Fatalf("expected first EKSIRSA reconcile to persist status before remote patch, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected transient requeue after persisting EKSIRSA delivery context, got %#v", result)
	}

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	if stored.Status.RoleARN != testRoleARN ||
		stored.Status.DeliveryType != identityv1.DeliveryTypeEKSIRSA ||
		stored.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected persisted EKSIRSA deletion context, got status=%#v", stored.Status)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending)

	storedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(storedSA), storedSA); err != nil {
		t.Fatal(err)
	}

	if storedSA.Annotations[selfHostedRoleARNAnnotation] != "" {
		t.Fatalf("expected first EKSIRSA pass not to patch remote annotations before status persistence, got %#v", storedSA.Annotations)
	}
}

func TestRoleReconcileNormalReportsDuplicateServiceAccountBindings(t *testing.T) {
	roleA := roleForServiceAccountInNamespace("app-a", testInventoryNamespace, "app")
	roleB := roleForServiceAccountInNamespace("app-b", testInventoryNamespace, "app")
	localClient := testConfigClient(t, roleA, roleB)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSServiceAccountRoleReconciler{Client: localClient, Recorder: recorder}

	for _, role := range []*identityv1.AWSServiceAccountRole{roleA, roleB} {
		result, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role)
		if err != nil {
			t.Fatalf("expected duplicate binding to be reported in status, got result=%#v err=%v", result, err)
		}

		if result.RequeueAfter != transientRequeue {
			t.Fatalf("expected conflict to recheck after %s, got %#v", transientRequeue, result)
		}
	}

	for _, name := range []string{"app-a", "app-b"} {
		stored := &identityv1.AWSServiceAccountRole{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testInventoryNamespace}}
		if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(stored), stored); err != nil {
			t.Fatal(err)
		}

		assertCondition(t, stored.Status.Conditions, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
		assertCondition(t, stored.Status.Conditions, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)
	}

	if len(recorder.events) != 4 {
		t.Fatalf("expected duplicate conflict to emit warning events for both roles, got %#v", recorder.events)
	}

	for i, event := range recorder.events {
		if event.eventType != corev1.EventTypeWarning ||
			event.reason != identityv1.ReasonInvalidSpec ||
			event.action != eventActionConditionTransitioned {
			t.Fatalf("unexpected conflict event %d: %#v", i, event)
		}
	}
}

func TestRolesForSiblingServiceAccountBindingEnqueuesConflictingSibling(t *testing.T) {
	roleA := roleForServiceAccount("role-a", "app")
	roleB := roleForServiceAccount("role-b", "app")
	roleOther := roleForServiceAccount("role-other", "other-sa")

	localClient := testConfigClient(t, roleA, roleB, roleOther)
	reconciler := &AWSServiceAccountRoleReconciler{Client: localClient}

	requests := reconciler.rolesForSiblingServiceAccountBinding(context.Background(), roleB)

	expected := client.ObjectKeyFromObject(roleA)
	if len(requests) != 1 || requests[0].NamespacedName != expected {
		t.Fatalf("expected sibling enqueue to return only %s, got %#v", expected, requests)
	}
}

// A deleted sibling is no longer in the cache; the controller still enqueues
// the surviving sibling so its duplicate-binding conflict can clear without
// waiting for transientRequeue.
func TestRolesForSiblingServiceAccountBindingHandlesDeletedSibling(t *testing.T) {
	roleA := roleForServiceAccount("role-a", "app")
	deletedB := roleForServiceAccount("role-b", "app")

	localClient := testConfigClient(t, roleA)
	reconciler := &AWSServiceAccountRoleReconciler{Client: localClient}

	requests := reconciler.rolesForSiblingServiceAccountBinding(context.Background(), deletedB)

	expected := client.ObjectKeyFromObject(roleA)
	if len(requests) != 1 || requests[0].NamespacedName != expected {
		t.Fatalf("expected deletion event to map to surviving sibling %s, got %#v", expected, requests)
	}
}

func TestRolesForSiblingServiceAccountBindingIgnoresEmptyServiceAccount(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "role", Namespace: testInventoryNamespace},
	}

	reconciler := &AWSServiceAccountRoleReconciler{Client: testConfigClient(t)}

	if requests := reconciler.rolesForSiblingServiceAccountBinding(context.Background(), role); len(requests) != 0 {
		t.Fatalf("expected empty ServiceAccount binding to enqueue nothing, got %#v", requests)
	}
}

// Event-driven conflict clear: once a sibling enters termination, the
// surviving role's next reconcile must clear ConditionDeliveryReady from
// ReasonInvalidSpec without depending on the 30s transientRequeue.
func TestRoleReconcileNormalClearsDuplicateConflictAfterSiblingDeletion(t *testing.T) {
	roleA := roleForServiceAccount("role-a", "app")
	roleA.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}
	roleB := roleForServiceAccount("role-b", "app")
	roleB.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}
	localClient := testConfigClient(t, roleA, roleB)
	recorder := &capturingEventRecorder{}
	reconciler := &AWSServiceAccountRoleReconciler{Client: localClient, Recorder: recorder}

	for _, role := range []*identityv1.AWSServiceAccountRole{roleA, roleB} {
		if _, err := reconciler.reconcileNormal(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
			t.Fatalf("setup conflict: %v", err)
		}
	}

	storedA := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(roleA), storedA); err != nil {
		t.Fatal(err)
	}

	assertCondition(t, storedA.Status.Conditions, identityv1.ConditionDeliveryReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec)

	if err := localClient.Delete(context.Background(), roleB); err != nil {
		t.Fatalf("delete sibling: %v", err)
	}

	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(roleA), storedA); err != nil {
		t.Fatal(err)
	}

	result, done, err := reconciler.reconcileDuplicateBindingConflict(context.Background(), logr.Discard(), storedA, storedA.Status.DeepCopy())
	if err != nil {
		t.Fatalf("reconcile after sibling deletion: %v", err)
	}

	if done {
		t.Fatalf("expected conflict-detection step to release the pipeline once sibling was deleted, got done=true result=%#v", result)
	}

	if result.RequeueAfter == transientRequeue {
		t.Fatalf("expected conflict to be cleared without transient requeue, got %#v", result)
	}
}

func TestSetDeliveryConditionsRetriesMissingRemoteServiceAccount(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testInventoryNamespace},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
		},
		Status: identityv1.AWSServiceAccountRoleStatus{
			RoleARN: testRoleARN,
		},
	}
	reconciler := &AWSServiceAccountRoleReconciler{
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
	}

	result, err := reconciler.setDeliveryConditions(context.Background(), logr.Discard(), role, identityv1.DeliveryTypeSelfHostedIRSA, &roleReconcileInputs{
		resolved: inventory.Resolution{
			ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
			Ready:       true,
		},
	}, nil)
	if err != nil {
		t.Fatalf("expected missing remote ServiceAccount to be handled as a fixed retry, got result=%#v err=%v", result, err)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected missing ServiceAccount to retry after %s, got %s", transientRequeue, result.RequeueAfter)
	}

	assertCondition(t, role.Status.Conditions, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionFalse, identityv1.ReasonRemoteDeliveryPending)
}

func TestSetRoleResolvedDeliveryStatusRecordsDeletionContext(t *testing.T) {
	role := testAWSServiceAccountRole()
	setRoleResolvedDeliveryStatus(role, identityv1.DeliveryTypeSelfHostedIRSA, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})

	if role.Status.DeliveryType != identityv1.DeliveryTypeSelfHostedIRSA {
		t.Fatalf("expected self-hosted delivery type, got %q", role.Status.DeliveryType)
	}

	if role.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name %q, got %q", testResolvedClusterName, role.Status.ResolvedClusterName)
	}

	setRoleResolvedDeliveryStatus(role, identityv1.DeliveryTypeEKSPodIdentity, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})

	if role.Status.DeliveryType != identityv1.DeliveryTypeEKSPodIdentity {
		t.Fatalf("expected EKS delivery type, got %q", role.Status.DeliveryType)
	}

	if role.Status.ResolvedClusterName != "" {
		t.Fatalf("expected non-self-hosted delivery to clear resolved cluster name, got %q", role.Status.ResolvedClusterName)
	}
}

type roleDeleteCleanupBlockedCase struct {
	name        string
	objects     func(*identityv1.AWSServiceAccountRole) []client.Object
	resolver    func(client.Client) inventory.Resolver
	manager     mcmanager.Manager
	wantErrText string
}

func roleDeleteCleanupBlockedCases(t *testing.T) []roleDeleteCleanupBlockedCase {
	t.Helper()

	defaultResolver := func(c client.Client) inventory.Resolver { return inventory.Resolver{Client: c} }

	return []roleDeleteCleanupBlockedCase{
		{
			name: "config unavailable without recorded delivery state",
			objects: func(role *identityv1.AWSServiceAccountRole) []client.Object {
				return []client.Object{role}
			},
			manager:     &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
			wantErrText: "status.deliveryType is empty",
		},
		{
			name: "resolver error",
			objects: func(role *identityv1.AWSServiceAccountRole) []client.Object {
				return []client.Object{role, testSelfHostedConfig()}
			},
			manager:     &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
			wantErrText: "resolve inventory",
		},
		{
			name: "inventory not ready",
			objects: func(role *identityv1.AWSServiceAccountRole) []client.Object {
				return []client.Object{role, testSelfHostedConfig()}
			},
			resolver:    defaultResolver,
			manager:     &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
			wantErrText: "inventory not ready",
		},
		{
			name: "remote cluster unavailable",
			objects: func(role *identityv1.AWSServiceAccountRole) []client.Object {
				return []client.Object{role, testSelfHostedConfig(), testResolvedClusterProfile(role.Namespace)}
			},
			resolver:    defaultResolver,
			manager:     &testRoleManager{getter: &testRemoteClusterGetter{err: errors.New("cluster unavailable")}},
			wantErrText: "resolve remote cluster client",
		},
		{
			name: "multicluster manager unavailable",
			objects: func(role *identityv1.AWSServiceAccountRole) []client.Object {
				return []client.Object{role, testSelfHostedConfig(), testResolvedClusterProfile(role.Namespace)}
			},
			resolver: func(c client.Client) inventory.Resolver {
				return inventory.Resolver{Client: c}
			},
			wantErrText: "multicluster manager is not configured",
		},
	}
}

func TestRoleDeleteBlocksWhenSelfHostedAnnotationCleanupCannotResolveTarget(t *testing.T) {
	for _, tt := range roleDeleteCleanupBlockedCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			role := testFinalizedAWSServiceAccountRoleWithARN()
			localClient := testConfigClient(t, tt.objects(role)...)

			resolver := inventory.Resolver{}
			if tt.resolver != nil {
				resolver = tt.resolver(localClient)
			}

			reconciler := &AWSServiceAccountRoleReconciler{
				Client:    localClient,
				MCManager: tt.manager,
				Resolver:  resolver,
			}

			err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role)
			if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
				t.Fatalf("expected delete to fail with %q, got %v", tt.wantErrText, err)
			}

			assertStoredRoleFinalizer(t, localClient, role, true)
		})
	}
}

// Regression: when remote annotation cleanup fails because the
// inventory cannot resolve the workload cluster, reconcileDelete must persist
// a ConditionDeletionBlocked=True signal with the upstream error verbatim so
// operators can observe why finalization is paused.
func TestRoleDeleteMarksDeletionBlockedWhenRemoteAnnotationCleanupFails(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	// "inventory not ready" path: config present, resolver wired, but no
	// ClusterProfile so inventory.Resolve returns Ready=false. This exercises
	// cleanupRemoteServiceAccountAnnotations -> resolveInventory.Ready==false,
	// the canonical "remote cluster unavailable" failure surface.
	localClient := testConfigClient(t, role, testSelfHostedConfig())
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
		Resolver:  inventory.Resolver{Client: localClient},
	}

	err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role)
	if err == nil {
		t.Fatal("expected reconcileDelete to return the cleanup error so the manager requeues")
	}

	wantErrText := "inventory not ready"
	if !strings.Contains(err.Error(), wantErrText) {
		t.Fatalf("expected cleanup error containing %q, got %v", wantErrText, err)
	}

	assertStoredRoleFinalizer(t, localClient, role, true)

	stored := &identityv1.AWSServiceAccountRole{}
	if getErr := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); getErr != nil {
		t.Fatal(getErr)
	}

	cond := meta.FindStatusCondition(stored.Status.Conditions, identityv1.ConditionDeletionBlocked)
	if cond == nil {
		t.Fatalf("expected %s condition to be recorded so observers see deletion is paused, got %#v", identityv1.ConditionDeletionBlocked, stored.Status.Conditions)
	}

	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected %s=True, got %s", identityv1.ConditionDeletionBlocked, cond.Status)
	}

	if cond.Reason != identityv1.ReasonRemoteClusterUnavailable {
		t.Fatalf("expected reason %s, got %s", identityv1.ReasonRemoteClusterUnavailable, cond.Reason)
	}

	if !strings.Contains(cond.Message, err.Error()) {
		t.Fatalf("expected DeletionBlocked message to surface the upstream error %q, got %q", err.Error(), cond.Message)
	}
}

// Regression: once cleanup succeeds, reconcileDelete must
// transition a previously-True DeletionBlocked condition to False with
// ReasonDeletionUnblocked before removing the finalizer so observers
// (and metric allowlists) see the unblock event.
func TestRoleDeleteClearsDeletionBlockedAfterCleanSuccess(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	role.Status.DeliveryType = identityv1.DeliveryTypeSelfHostedIRSA
	role.Status.ResolvedClusterName = testResolvedClusterName
	// Pre-seed a stuck DeletionBlocked=True condition so the clear path is
	// exercised. A previous reconcile attempt would have written this when the
	// remote cluster was unreachable.
	setCondition(
		&role.Status.Conditions,
		role.Generation,
		identityv1.ConditionDeletionBlocked,
		metav1.ConditionTrue,
		identityv1.ReasonRemoteClusterUnavailable,
		"remote cluster previously unreachable",
	)

	remoteServiceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        role.Spec.ServiceAccount.Name,
			Namespace:   role.Spec.ServiceAccount.Namespace,
			Annotations: renderSelfHostedServiceAccountAnnotations(testRoleARN),
		},
	}
	remoteClient := fakeClient(t, remoteServiceAccount)
	localClient := testConfigClient(t, role)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: remoteClient}},
	}

	if err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected clean delete path to succeed, got %v", err)
	}

	assertStoredRoleFinalizer(t, localClient, role, false)

	stored := &identityv1.AWSServiceAccountRole{}
	if err := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(stored.Status.Conditions, identityv1.ConditionDeletionBlocked)
	if cond == nil {
		t.Fatalf("expected %s condition to remain on object so observers see the False transition, got %#v", identityv1.ConditionDeletionBlocked, stored.Status.Conditions)
	}

	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected %s=False after successful cleanup, got %s", identityv1.ConditionDeletionBlocked, cond.Status)
	}

	if cond.Reason != identityv1.ReasonDeletionUnblocked {
		t.Fatalf("expected reason %s after successful cleanup, got %s", identityv1.ReasonDeletionUnblocked, cond.Reason)
	}
}

func TestRoleDeleteDoesNotStrandFinalizerWhenConfigWasForceDeleted(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	role.Status.DeliveryType = identityv1.DeliveryTypeSelfHostedIRSA
	role.Status.ResolvedClusterName = testResolvedClusterName
	remoteServiceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        role.Spec.ServiceAccount.Name,
			Namespace:   role.Spec.ServiceAccount.Namespace,
			Annotations: renderSelfHostedServiceAccountAnnotations(testRoleARN),
		},
	}
	remoteClient := fakeClient(t, remoteServiceAccount)
	localClient := testConfigClient(t, role)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: remoteClient}},
	}

	if err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected role delete to continue after config force-delete, got %v", err)
	}

	assertStoredRoleFinalizer(t, localClient, role, false)

	storedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(storedSA), storedSA); err != nil {
		t.Fatal(err)
	}

	if storedSA.Annotations[selfHostedRoleARNAnnotation] != "" {
		t.Fatalf("expected recorded self-hosted cleanup to remove role annotation, got %#v", storedSA.Annotations)
	}
}

func TestRoleDeleteUsesRecordedDeliveryStatusBeforeCurrentConfig(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	role.Status.DeliveryType = identityv1.DeliveryTypeSelfHostedIRSA
	role.Status.ResolvedClusterName = testResolvedClusterName
	currentConfig := testSelfHostedConfig()
	currentConfig.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	remoteServiceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        role.Spec.ServiceAccount.Name,
			Namespace:   role.Spec.ServiceAccount.Namespace,
			Annotations: renderSelfHostedServiceAccountAnnotations(testRoleARN),
		},
	}
	remoteClient := fakeClient(t, remoteServiceAccount)
	localClient := testConfigClient(t, role, currentConfig)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: remoteClient}},
	}

	if err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected role delete to use recorded delivery status, got %v", err)
	}

	storedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(storedSA), storedSA); err != nil {
		t.Fatal(err)
	}

	if storedSA.Annotations[selfHostedRoleARNAnnotation] != "" {
		t.Fatalf("expected recorded self-hosted cleanup to ignore current config type and remove annotation, got %#v", storedSA.Annotations)
	}
}

func TestCleanupRemoteServiceAccountAnnotationsNoOpsWhenCleanupIsNotRequired(t *testing.T) {
	tests := []struct {
		name    string
		role    func() *identityv1.AWSServiceAccountRole
		objects func(*identityv1.AWSServiceAccountRole) []client.Object
		manager mcmanager.Manager
	}{
		{
			name: "empty RoleARN",
			role: testAWSServiceAccountRole,
			objects: func(role *identityv1.AWSServiceAccountRole) []client.Object {
				return []client.Object{role, testSelfHostedConfig()}
			},
			manager: &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
		},
		{
			name: "non self-hosted delivery",
			role: testFinalizedAWSServiceAccountRoleWithARN,
			objects: func(role *identityv1.AWSServiceAccountRole) []client.Object {
				config := testSelfHostedConfig()
				config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity

				return []client.Object{role, config}
			},
			manager: &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := tt.role()
			localClient := testConfigClient(t, tt.objects(role)...)
			reconciler := &AWSServiceAccountRoleReconciler{
				Client:    localClient,
				MCManager: tt.manager,
			}

			if err := reconciler.cleanupRemoteServiceAccountAnnotations(context.Background(), logr.Discard(), role); err != nil {
				t.Fatalf("expected cleanup no-op, got %v", err)
			}
		})
	}
}

func TestCleanupRemoteServiceAccountAnnotationsTreatsMissingRemoteServiceAccountAsClean(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	config := testSelfHostedConfig()
	profile := testResolvedClusterProfile(role.Namespace)
	localClient := testConfigClient(t, role, config, profile)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
		Resolver:  inventory.Resolver{Client: localClient},
	}

	if err := reconciler.cleanupRemoteServiceAccountAnnotations(context.Background(), logr.Discard(), role); err != nil {
		t.Fatalf("expected missing remote ServiceAccount to count as clean, got %v", err)
	}
}

func TestTrustPolicyUsesEKSIRSAIssuerStatus(t *testing.T) {
	role := testAWSServiceAccountRole()
	config := testRoleReadyEKSIRSAConfig()

	policy, err := (&AWSServiceAccountRoleReconciler{}).trustPolicy(role, config, &inventory.Resolution{Ready: true})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(policy, `"Federated":"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE"`) {
		t.Fatalf("expected EKS OIDC provider principal, got %s", policy)
	}

	if !strings.Contains(policy, `"oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE:sub":"system:serviceaccount:default:app"`) {
		t.Fatalf("expected EKS issuer subject condition, got %s", policy)
	}
}

func TestComputeRoleReadyStateRequiresConfigReadyForEKSIRSA(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{}
	config := testRoleReadyEKSIRSAConfig()

	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionRoleReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionFalse, identityv1.ReasonInvalidSpec, "invalid")

	status, reason, _ := computeRoleReadyState(role, identityv1.DeliveryTypeEKSIRSA, config)
	if status != metav1.ConditionFalse || reason != identityv1.ReasonConfigNotReady {
		t.Fatalf("expected ConfigNotReady, got status=%s reason=%s", status, reason)
	}
}

func roleWithHubResourcesReady() *identityv1.AWSServiceAccountRole {
	role := &identityv1.AWSServiceAccountRole{}
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionRoleReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPolicyReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	return role
}

type eksPodIdentityLadderCase struct {
	name                      string
	assocStatus               metav1.ConditionStatus
	assocReason               string
	assocMessage              string
	agentSet                  bool
	agentStatus               metav1.ConditionStatus
	agentReason, agentMessage string
	wantStatus                metav1.ConditionStatus
	wantReason, wantMsgSubstr string
}

var eksPodIdentityLadderCases = []eksPodIdentityLadderCase{
	{
		name:        "ACKPendingSurfacesWaitingForACK",
		assocStatus: metav1.ConditionFalse, assocReason: identityv1.ReasonWaitingForACK, assocMessage: "ACK PodIdentityAssociation not yet synced",
		wantStatus: metav1.ConditionFalse, wantReason: identityv1.ReasonWaitingForACK, wantMsgSubstr: "PodIdentityAssociation",
	},
	{
		name:        "RemoteAgentUnknownSurfacesRemoteCheckPending",
		assocStatus: metav1.ConditionTrue, assocReason: identityv1.ReasonReady, assocMessage: "ready",
		agentSet: true, agentStatus: metav1.ConditionUnknown, agentReason: identityv1.ReasonRemoteCheckPending, agentMessage: "probe pending",
		wantStatus: metav1.ConditionFalse, wantReason: identityv1.ReasonRemoteCheckPending,
	},
	{
		name:        "MissingAgentConditionSurfacesRemoteCheckPending",
		assocStatus: metav1.ConditionTrue, assocReason: identityv1.ReasonReady, assocMessage: "ready",
		wantStatus: metav1.ConditionFalse, wantReason: identityv1.ReasonRemoteCheckPending,
	},
	{
		name:        "RemoteAgentFalseSurfacesRemoteDeliveryPending",
		assocStatus: metav1.ConditionTrue, assocReason: identityv1.ReasonReady, assocMessage: "ready",
		agentSet: true, agentStatus: metav1.ConditionFalse, agentReason: identityv1.ReasonRemoteDeliveryPending, agentMessage: "agent daemonset not Ready",
		wantStatus: metav1.ConditionFalse, wantReason: identityv1.ReasonRemoteDeliveryPending,
	},
	{
		name:        "AllReadyReportsHubResourcesReady",
		assocStatus: metav1.ConditionTrue, assocReason: identityv1.ReasonReady, assocMessage: "ready",
		agentSet: true, agentStatus: metav1.ConditionTrue, agentReason: identityv1.ReasonReady, agentMessage: "agent ready",
		wantStatus: metav1.ConditionTrue, wantReason: identityv1.ReasonHubResourcesReady,
	},
}

// Pins the discriminated EKSPodIdentity ladder so each rung (ACK pending,
// remote Unknown, remote absent, remote False, all Ready) reports a distinct
// reason — operators rely on those to tell hub-side from remote-side waits.
func TestComputeRoleReadyStateEKSPodIdentityLadder(t *testing.T) {
	for _, tc := range eksPodIdentityLadderCases {
		t.Run(tc.name, func(t *testing.T) {
			role := roleWithHubResourcesReady()
			setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAssocReady, tc.assocStatus, tc.assocReason, tc.assocMessage)

			if tc.agentSet {
				setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAgentReady, tc.agentStatus, tc.agentReason, tc.agentMessage)
			}

			status, reason, message := computeRoleReadyState(role, identityv1.DeliveryTypeEKSPodIdentity, &identityv1.AWSWorkloadIdentityConfig{})
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("got status=%s reason=%s, want status=%s reason=%s", status, reason, tc.wantStatus, tc.wantReason)
			}

			if tc.wantMsgSubstr != "" && !strings.Contains(message, tc.wantMsgSubstr) {
				t.Fatalf("expected message to contain %q, got %q", tc.wantMsgSubstr, message)
			}
		})
	}
}

// setHubReadyConditions remaps the underlying HubResourcesReady reason to the
// canonical ReasonReady on Ready while keeping DeliveryReady on
// HubResourcesReady — verify the lockstep so external consumers keep seeing
// "Ready/Ready" once the EKSPodIdentity sub-conditions converge.
func TestSetHubReadyConditionsEKSPodIdentityAllReadyMapsToReadyReason(t *testing.T) {
	role := roleWithHubResourcesReady()
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAssocReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionPodIdentityAgentReady, metav1.ConditionTrue, identityv1.ReasonReady, "agent ready")

	setHubReadyConditions(role, identityv1.DeliveryTypeEKSPodIdentity, &identityv1.AWSWorkloadIdentityConfig{})

	assertCondition(t, role.Status.Conditions, identityv1.ConditionDeliveryReady, metav1.ConditionTrue, identityv1.ReasonHubResourcesReady)
	assertCondition(t, role.Status.Conditions, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady)
}

func TestSetRoleResolvedDeliveryStatusRecordsEKSIRSADeletionContext(t *testing.T) {
	role := testAWSServiceAccountRole()
	setRoleResolvedDeliveryStatus(role, identityv1.DeliveryTypeEKSIRSA, &inventory.Resolution{
		ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
		Ready:       true,
	})

	if role.Status.DeliveryType != identityv1.DeliveryTypeEKSIRSA {
		t.Fatalf("expected EKSIRSA delivery type, got %q", role.Status.DeliveryType)
	}

	if role.Status.ResolvedClusterName != testResolvedClusterName {
		t.Fatalf("expected resolved cluster name %q, got %q", testResolvedClusterName, role.Status.ResolvedClusterName)
	}
}

func TestSetDeliveryConditionsPatchesRemoteServiceAccountForEKSIRSA(t *testing.T) {
	role := testAWSServiceAccountRole()
	role.Status.RoleARN = testRoleARN
	remoteServiceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	remoteClient := fakeClient(t, remoteServiceAccount)
	reconciler := &AWSServiceAccountRoleReconciler{
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: remoteClient}},
	}
	beforeEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))
	beforeSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))

	result, err := reconciler.setDeliveryConditions(context.Background(), logr.Discard(), role, identityv1.DeliveryTypeEKSIRSA, &roleReconcileInputs{
		resolved: inventory.Resolution{
			ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
			Ready:       true,
		},
	}, &identityv1.AWSServiceAccountRoleStatus{})
	if err != nil {
		t.Fatalf("expected EKSIRSA delivery to patch ServiceAccount, got result=%#v err=%v", result, err)
	}

	storedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(storedSA), storedSA); err != nil {
		t.Fatal(err)
	}

	if storedSA.Annotations[selfHostedRoleARNAnnotation] != testRoleARN {
		t.Fatalf("expected role ARN annotation, got %#v", storedSA.Annotations)
	}

	assertCondition(t, role.Status.Conditions, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady)

	afterEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))

	afterSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))
	if got := afterEKSIRSA - beforeEKSIRSA; got != 1 {
		t.Fatalf("expected EKSIRSA annotation apply metric delta 1, got %v", got)
	}

	if got := afterSelfHosted - beforeSelfHosted; got != 0 {
		t.Fatalf("expected SelfHostedIRSA annotation apply metric delta 0, got %v", got)
	}
}

func TestRoleDeleteUsesRecordedEKSIRSADeliveryStatus(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	role.Status.DeliveryType = identityv1.DeliveryTypeEKSIRSA
	role.Status.ResolvedClusterName = testResolvedClusterName
	remoteServiceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        role.Spec.ServiceAccount.Name,
			Namespace:   role.Spec.ServiceAccount.Namespace,
			Annotations: renderSelfHostedServiceAccountAnnotations(testRoleARN),
		},
	}
	remoteClient := fakeClient(t, remoteServiceAccount)
	localClient := testConfigClient(t, role)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:    localClient,
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: remoteClient}},
	}
	beforeEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))
	beforeSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))

	if err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("expected role delete to clean EKSIRSA annotations, got %v", err)
	}

	storedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: role.Spec.ServiceAccount.Name, Namespace: role.Spec.ServiceAccount.Namespace}}
	if err := remoteClient.Get(context.Background(), client.ObjectKeyFromObject(storedSA), storedSA); err != nil {
		t.Fatal(err)
	}

	if storedSA.Annotations[selfHostedRoleARNAnnotation] != "" {
		t.Fatalf("expected recorded EKSIRSA cleanup to remove role annotation, got %#v", storedSA.Annotations)
	}

	afterEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))

	afterSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.RemoteDeliveryResultSuccess, string(controllerutil.OperationResultUpdated))
	if got := afterEKSIRSA - beforeEKSIRSA; got != 1 {
		t.Fatalf("expected EKSIRSA annotation cleanup metric delta 1, got %v", got)
	}

	if got := afterSelfHosted - beforeSelfHosted; got != 0 {
		t.Fatalf("expected SelfHostedIRSA annotation cleanup metric delta 0, got %v", got)
	}
}

func testAWSServiceAccountRole() *identityv1.AWSServiceAccountRole {
	return &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: testInventoryNamespace,
			UID:       types.UID(testRoleUID),
		},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
		},
	}
}

func testFinalizedAWSServiceAccountRoleWithARN() *identityv1.AWSServiceAccountRole {
	role := testAWSServiceAccountRole()
	role.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}
	role.Status.RoleARN = testRoleARN

	return role
}

func testRoleReadySelfHostedConfig() *identityv1.AWSWorkloadIdentityConfig {
	config := testSelfHostedConfig()
	config.Status.OIDCProviderARN = "arn:aws:iam::123456789012:oidc-provider/example"
	config.Status.IssuerHostPath = "example.s3.ap-northeast-1.amazonaws.com"
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	return config
}

func testRoleReadyEKSIRSAConfig() *identityv1.AWSWorkloadIdentityConfig {
	config := testEKSIRSAConfig(
		identityv1.OIDCProviderManagementExternal,
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	)
	config.Status.OIDCProviderARN = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE"
	config.Status.IssuerHostPath = "oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE"
	setCondition(&config.Status.Conditions, config.Generation, identityv1.ConditionReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	return config
}

func testIAMRoleWithARN(role *identityv1.AWSServiceAccountRole, arn string) *iamv1alpha1.Role {
	resourceARN := ackv1alpha1.AWSResourceName(arn)

	return &iamv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace},
		Status: iamv1alpha1.RoleStatus{
			ACKResourceMetadata: &ackv1alpha1.ResourceMetadata{ARN: &resourceARN},
			Conditions: []*ackv1alpha1.Condition{{
				Type:   ackv1alpha1.ConditionTypeResourceSynced,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func assertStoredRoleFinalizer(t *testing.T, c client.Client, role *identityv1.AWSServiceAccountRole, want bool) {
	t.Helper()

	stored := &identityv1.AWSServiceAccountRole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), stored); err != nil {
		t.Fatal(err)
	}

	got := controllerutil.ContainsFinalizer(stored, identityv1.ServiceAccountRoleFinalizer)
	if got != want {
		t.Fatalf("expected finalizer present=%t, got %t: %#v", want, got, stored.Finalizers)
	}
}

type testRoleManager struct {
	mcmanager.Manager
	getter *testRemoteClusterGetter
}

func (m *testRoleManager) GetCluster(ctx context.Context, clusterName multicluster.ClusterName) (cluster.Cluster, error) {
	return m.getter.GetCluster(ctx, clusterName)
}

func TestServiceAccountAnnotationSyncReasonRecordsRepairOnlyForReadyDrift(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testInventoryNamespace}}
	beforeStatus := &identityv1.AWSServiceAccountRoleStatus{}
	setCondition(&beforeStatus.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	recorder := &capturingEventRecorder{}

	reason := serviceAccountAnnotationSyncReason(recorder, role, beforeStatus, controllerutil.OperationResultUpdated)
	if reason != identityv1.ReasonAnnotationRepaired {
		t.Fatalf("expected repair reason, got %q", reason)
	}

	expected := []recordedEvent{{
		regarding: role,
		eventType: corev1.EventTypeNormal,
		reason:    identityv1.ReasonAnnotationRepaired,
		action:    eventActionRepairAnnotation,
		note:      eventNoteAnnotationRepaired,
	}}
	assertRecordedEvents(t, recorder.events, expected)
}

func TestServiceAccountAnnotationSyncReasonDoesNotRecordInitialSync(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testInventoryNamespace}}
	recorder := &capturingEventRecorder{}

	reason := serviceAccountAnnotationSyncReason(recorder, role, &identityv1.AWSServiceAccountRoleStatus{}, controllerutil.OperationResultUpdated)
	if reason != identityv1.ReasonReady {
		t.Fatalf("expected ready reason, got %q", reason)
	}

	if len(recorder.events) != 0 {
		t.Fatalf("expected no repair event on initial sync, got %#v", recorder.events)
	}
}

// stampRoleControllerRef writes a controller-style OwnerReference referencing
// owner onto child. We stamp it explicitly (rather than via
// controllerutil.SetControllerReference) so the UID under test is unambiguous
// in the test source.
func stampRoleControllerRef(role *identityv1.AWSServiceAccountRole, child client.Object, ownerUID types.UID) {
	truePtr := true
	child.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         identityv1.GroupVersion.String(),
		Kind:               "AWSServiceAccountRole",
		Name:               role.Name,
		UID:                ownerUID,
		Controller:         &truePtr,
		BlockOwnerDeletion: &truePtr,
	}})
}

// TestRoleReconcileDeleteRemovesOwnedACKChildren is the ACK-child deletion-order regression test
// for the "happy path": when the generated ACK CRs carry a controllerRef whose
// UID matches the role's UID, reconcileDelete must remove all three children
// and then release the role finalizer.
func TestRoleReconcileDeleteRemovesOwnedACKChildren(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	// EKSPodIdentity makes cleanupRemoteServiceAccountAnnotations a no-op
	// (see role_controller.go cleanupRemoteServiceAccountAnnotationsFromRecordedStatus)
	// so the test focuses on ACK child deletion alone.
	role.Status.DeliveryType = identityv1.DeliveryTypeEKSPodIdentity

	pia := &eksv1alpha1.PodIdentityAssociation{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.PodIdentityAssociationName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, pia, role.UID)

	iamRole := &iamv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, iamRole, role.UID)

	iamPolicy := &iamv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, iamPolicy, role.UID)

	localClient := testConfigClient(t, role, pia, iamRole, iamPolicy)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Recorder: &capturingEventRecorder{},
	}

	if err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	assertACKChildAbsent(t, localClient, &eksv1alpha1.PodIdentityAssociation{}, client.ObjectKeyFromObject(pia))
	assertACKChildAbsent(t, localClient, &iamv1alpha1.Role{}, client.ObjectKeyFromObject(iamRole))
	assertACKChildAbsent(t, localClient, &iamv1alpha1.Policy{}, client.ObjectKeyFromObject(iamPolicy))

	assertStoredRoleFinalizer(t, localClient, role, false)
}

// TestRoleReconcileDeleteSkipsACKChildrenOwnedByDifferentRole is the ACK-child deletion-order
// regression test for foreign ownership: an ACK CR sharing the generated name
// but whose controllerRef points at a *different* role UID must NOT be
// collateral-deleted. The role's own finalizer should still be released so
// orphan ACK CRs do not strand role cleanup.
func TestRoleReconcileDeleteSkipsACKChildrenOwnedByDifferentRole(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	role.Status.DeliveryType = identityv1.DeliveryTypeEKSPodIdentity

	foreignUID := types.UID("some-other-role-uid")

	pia := &eksv1alpha1.PodIdentityAssociation{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.PodIdentityAssociationName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, pia, foreignUID)

	iamRole := &iamv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, iamRole, foreignUID)

	iamPolicy := &iamv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, iamPolicy, foreignUID)

	localClient := testConfigClient(t, role, pia, iamRole, iamPolicy)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Recorder: &capturingEventRecorder{},
	}

	if err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	assertACKChildPresent(t, localClient, &eksv1alpha1.PodIdentityAssociation{}, client.ObjectKeyFromObject(pia))
	assertACKChildPresent(t, localClient, &iamv1alpha1.Role{}, client.ObjectKeyFromObject(iamRole))
	assertACKChildPresent(t, localClient, &iamv1alpha1.Policy{}, client.ObjectKeyFromObject(iamPolicy))

	assertStoredRoleFinalizer(t, localClient, role, false)
}

// TestRoleReconcileDeleteSkipsACKChildrenWithoutControllerRef is the ACK-child deletion-order
// regression test for resources the operator never authored: an ACK CR
// happening to share the generated name but with no controllerRef at all must
// be left untouched. The role's own finalizer should still be released.
func TestRoleReconcileDeleteSkipsACKChildrenWithoutControllerRef(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	role.Status.DeliveryType = identityv1.DeliveryTypeEKSPodIdentity

	pia := &eksv1alpha1.PodIdentityAssociation{
		ObjectMeta: metav1.ObjectMeta{
			Name:            identityaws.PodIdentityAssociationName(role),
			Namespace:       role.Namespace,
			OwnerReferences: []metav1.OwnerReference{},
		},
	}
	iamRole := &iamv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:            identityaws.RoleName(role),
			Namespace:       role.Namespace,
			OwnerReferences: []metav1.OwnerReference{},
		},
	}
	iamPolicy := &iamv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            identityaws.PolicyName(role),
			Namespace:       role.Namespace,
			OwnerReferences: []metav1.OwnerReference{},
		},
	}

	localClient := testConfigClient(t, role, pia, iamRole, iamPolicy)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Recorder: &capturingEventRecorder{},
	}

	if err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	assertACKChildPresent(t, localClient, &eksv1alpha1.PodIdentityAssociation{}, client.ObjectKeyFromObject(pia))
	assertACKChildPresent(t, localClient, &iamv1alpha1.Role{}, client.ObjectKeyFromObject(iamRole))
	assertACKChildPresent(t, localClient, &iamv1alpha1.Policy{}, client.ObjectKeyFromObject(iamPolicy))

	assertStoredRoleFinalizer(t, localClient, role, false)
}

// TestRoleReconcileDeleteHoldsFinalizerWhileOwnedACKChildrenStillPending is the
// ACK-child deletion-order regression test for the in-flight teardown case:
// when an owned ACK child still carries a finalizer (typically because ACK has
// not yet finished AWS-side teardown), reconcileDelete must NOT drop the
// parent finalizer. It must surface DeletionBlocked=True/ChildrenPending and
// return an error so the reconcile is requeued, ensuring the parent object
// survives until every owned child has actually left the API server.
func TestRoleReconcileDeleteHoldsFinalizerWhileOwnedACKChildrenStillPending(t *testing.T) {
	role := testFinalizedAWSServiceAccountRoleWithARN()
	role.Status.DeliveryType = identityv1.DeliveryTypeEKSPodIdentity

	pendingFinalizer := "ack.aws.com/test-pending"

	pia := &eksv1alpha1.PodIdentityAssociation{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.PodIdentityAssociationName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, pia, role.UID)
	pia.SetFinalizers([]string{pendingFinalizer})

	iamRole := &iamv1alpha1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.RoleName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, iamRole, role.UID)
	iamRole.SetFinalizers([]string{pendingFinalizer})

	iamPolicy := &iamv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: identityaws.PolicyName(role), Namespace: role.Namespace},
	}
	stampRoleControllerRef(role, iamPolicy, role.UID)
	iamPolicy.SetFinalizers([]string{pendingFinalizer})

	localClient := testConfigClient(t, role, pia, iamRole, iamPolicy)
	reconciler := &AWSServiceAccountRoleReconciler{
		Client:   localClient,
		Recorder: &capturingEventRecorder{},
	}

	err := reconciler.reconcileDelete(logr.NewContext(context.Background(), logr.Discard()), role)
	if err == nil {
		t.Fatalf("expected reconcileDelete to return an error while owned ACK children still hold finalizers, got nil")
	}

	if !strings.Contains(err.Error(), "waiting for ACK child resources to finish deletion") {
		t.Fatalf("expected error message to surface the waiting-for-ACK-children signal, got %q", err.Error())
	}

	// Children stay observable (with a DeletionTimestamp set by Delete) until
	// their finalizers are cleared — they must NOT be collateral-removed from
	// the API server while the parent is still around.
	assertACKChildPresent(t, localClient, &eksv1alpha1.PodIdentityAssociation{}, client.ObjectKeyFromObject(pia))
	assertACKChildPresent(t, localClient, &iamv1alpha1.Role{}, client.ObjectKeyFromObject(iamRole))
	assertACKChildPresent(t, localClient, &iamv1alpha1.Policy{}, client.ObjectKeyFromObject(iamPolicy))

	stored := &identityv1.AWSServiceAccountRole{}
	if getErr := localClient.Get(context.Background(), client.ObjectKeyFromObject(role), stored); getErr != nil {
		t.Fatalf("get role: %v", getErr)
	}

	assertCondition(t, stored.Status.Conditions, identityv1.ConditionDeletionBlocked, metav1.ConditionTrue, identityv1.ReasonChildrenPending)

	// The parent finalizer MUST still be present so the role object does not
	// vanish before its owned ACK children leave the API server.
	assertStoredRoleFinalizer(t, localClient, role, true)
}

func assertACKChildAbsent(t *testing.T, c client.Client, obj client.Object, key client.ObjectKey) {
	t.Helper()

	err := c.Get(context.Background(), key, obj)
	if err == nil {
		t.Fatalf("expected %T %s/%s to be deleted, but it is still present", obj, key.Namespace, key.Name)
	}

	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for %T %s/%s, got %v", obj, key.Namespace, key.Name, err)
	}
}

func assertACKChildPresent(t *testing.T, c client.Client, obj client.Object, key client.ObjectKey) {
	t.Helper()

	if err := c.Get(context.Background(), key, obj); err != nil {
		t.Fatalf("expected %T %s/%s to remain (foreign / no controllerRef), got %v", obj, key.Namespace, key.Name, err)
	}
}
