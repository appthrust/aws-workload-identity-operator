package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
)

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
	if eksResult.RequeueAfter != 0 {
		t.Fatalf("expected no EKS safety requeue, got %s", eksResult.RequeueAfter)
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
		Name:       "stale-policy",
	}}

	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	localClient := testConfigClient(t, role, testOperatorConfig(), config, testResolvedClusterProfile(role.Namespace))
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
}

func TestReconcileSelfHostedDeliveryRetriesMissingRemoteServiceAccount(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testInventoryNamespace},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
		},
		Status: identityv1.AWSServiceAccountRoleStatus{
			RoleARN: "arn:aws:iam::123456789012:role/app",
		},
	}
	reconciler := &AWSServiceAccountRoleReconciler{
		MCManager: &testRoleManager{getter: &testRemoteClusterGetter{client: fakeClient(t)}},
	}

	result, op, err := reconciler.reconcileSelfHostedDelivery(context.Background(), logr.Discard(), role, &roleReconcileInputs{
		resolved: inventory.Resolution{
			ClusterName: types.NamespacedName{Namespace: testInventoryNamespace, Name: testInventoryNamespace},
			Ready:       true,
		},
	})
	if err == nil {
		t.Fatal("expected missing remote ServiceAccount to return an error")
	}

	if op != controllerutil.OperationResultNone {
		t.Fatalf("expected no patch operation for missing ServiceAccount, got %s", op)
	}

	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected missing ServiceAccount to retry after %s, got %s", transientRequeue, result.RequeueAfter)
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
		reason:    eventReasonAnnotationRepaired,
		action:    eventActionRepairAnnotation,
		note:      "repaired remote ServiceAccount annotations",
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
