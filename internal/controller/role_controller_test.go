package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	iamv1alpha1 "github.com/aws-controllers-k8s/iam-controller/apis/v1alpha1"
	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

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

func TestRoleResultWithSelfHostedSafetyRequeue(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{}
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	result := roleResultWithSelfHostedSafetyRequeue(identityv1.DeliveryTypeSelfHostedIRSA, role, ctrl.Result{})
	if result.RequeueAfter != selfHostedSteadyStateRequeue {
		t.Fatalf("expected self-hosted safety requeue %s, got %s", selfHostedSteadyStateRequeue, result.RequeueAfter)
	}
}

func TestRoleResultWithSelfHostedSafetyRequeueSkipsNonSelfHostedOrExplicitResult(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{}
	setCondition(&role.Status.Conditions, role.Generation, identityv1.ConditionServiceAccountAnnotationReady, metav1.ConditionTrue, identityv1.ReasonReady, "ready")

	eksResult := roleResultWithSelfHostedSafetyRequeue(identityv1.DeliveryTypeEKSPodIdentity, role, ctrl.Result{})
	if eksResult.RequeueAfter != dependencySteadyStateRequeue {
		t.Fatalf("expected dependency safety requeue %s, got %s", dependencySteadyStateRequeue, eksResult.RequeueAfter)
	}

	explicit := ctrl.Result{RequeueAfter: 30 * time.Second}

	selfHostedResult := roleResultWithSelfHostedSafetyRequeue(identityv1.DeliveryTypeSelfHostedIRSA, role, explicit)
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
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetUID, IndexAWSServiceAccountRoleByReplicaSetUID).
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
		{identityv1.ConditionRoleReady, identityv1.ReasonReady},
	} {
		assertCondition(t, stored.Status.Conditions, tc.condType, metav1.ConditionTrue, tc.reason)
	}
}

// TestRoleReconcileNormalIAMRoleApplyErrorPreservesACKResources is the
// regression test for CR-01: previously reconcileNormal reset
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
	role.Spec.PolicyDocument = &apixv1.JSON{Raw: []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"*"}]}`)}

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
	// reset that motivated CR-01 would have overwritten it with an empty
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
		WithIndex(&identityv1.AWSServiceAccountRole{}, IndexRoleByReplicaSetUID, IndexAWSServiceAccountRoleByReplicaSetUID).
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
