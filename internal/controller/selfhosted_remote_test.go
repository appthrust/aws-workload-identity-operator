package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

func TestInventoryNamespaceFromCluster(t *testing.T) {
	tests := []struct {
		clusterName string
		want        string
	}{
		{clusterName: testResolvedClusterName, want: testInventoryNamespace},
		{clusterName: testInventoryNamespace, want: ""},
		{clusterName: "/" + testInventoryNamespace, want: ""},
		{clusterName: testInventoryNamespace + "/", want: ""},
		{clusterName: "", want: ""},
	}

	for _, tt := range tests {
		if got := inventoryNamespaceFromCluster(tt.clusterName); got != tt.want {
			t.Fatalf("inventoryNamespaceFromCluster(%q) = %q, want %q", tt.clusterName, got, tt.want)
		}
	}
}

func TestResolveAnnotationBasedIRSATargetAllowsEKSIRSA(t *testing.T) {
	ctx := context.Background()
	config := testEKSIRSAConfig(
		identityv1.OIDCProviderManagementExternal,
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	)
	localClient := testConfigClient(t, config, testResolvedClusterProfile(config.Namespace))
	remoteClient := fakeClient(t)

	target, result, err := resolveAnnotationBasedIRSATarget(ctx, &selfHostedTargetRequest{
		LocalClient: localClient,
		Resolver:    inventory.Resolver{Client: localClient},
		MCManager:   &testRemoteClusterGetter{client: remoteClient},
		ClusterName: multicluster.ClusterName(testResolvedClusterName),
		Namespace:   config.Namespace,
		Resource:    metrics.ResourceServiceAccount,
	}, logr.Discard())
	if err != nil {
		t.Fatalf("expected EKSIRSA annotation target to resolve, got target=%v result=%#v err=%v", target, result, err)
	}
	if target == nil || !result.IsZero() {
		t.Fatalf("expected target and empty result, got target=%v result=%#v", target, result)
	}
	if target.DeliveryType != identityv1.DeliveryTypeEKSIRSA {
		t.Fatalf("expected EKSIRSA delivery type, got %q", target.DeliveryType)
	}
	if target.Cluster == nil {
		t.Fatal("expected resolved cluster")
	}
}

func TestResolveAnnotationBasedIRSATargetAllowsSelfHostedIRSA(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	localClient := testConfigClient(t, config, testResolvedClusterProfile(config.Namespace))
	remoteClient := fakeClient(t)

	target, result, err := resolveAnnotationBasedIRSATarget(ctx, &selfHostedTargetRequest{
		LocalClient: localClient,
		Resolver:    inventory.Resolver{Client: localClient},
		MCManager:   &testRemoteClusterGetter{client: remoteClient},
		ClusterName: multicluster.ClusterName(testResolvedClusterName),
		Namespace:   config.Namespace,
		Resource:    metrics.ResourceServiceAccount,
	}, logr.Discard())
	if err != nil {
		t.Fatalf("expected SelfHostedIRSA annotation target to resolve, got target=%v result=%#v err=%v", target, result, err)
	}
	if target == nil || !result.IsZero() {
		t.Fatalf("expected target and empty result, got target=%v result=%#v", target, result)
	}
	if target.DeliveryType != identityv1.DeliveryTypeSelfHostedIRSA {
		t.Fatalf("expected SelfHostedIRSA delivery type, got %q", target.DeliveryType)
	}
	if target.Cluster == nil {
		t.Fatal("expected resolved cluster")
	}
}

func TestResolveAnnotationBasedIRSATargetSkipsEKSPodIdentity(t *testing.T) {
	ctx := context.Background()
	config := testSelfHostedConfig()
	config.Spec.Type = identityv1.DeliveryTypeEKSPodIdentity
	localClient := testConfigClient(t, config, testResolvedClusterProfile(config.Namespace))

	target, result, err := resolveAnnotationBasedIRSATarget(ctx, &selfHostedTargetRequest{
		LocalClient: localClient,
		Resolver:    inventory.Resolver{Client: localClient},
		MCManager:   &testRemoteClusterGetter{client: fakeClient(t)},
		ClusterName: multicluster.ClusterName(testResolvedClusterName),
		Namespace:   config.Namespace,
		Resource:    metrics.ResourceServiceAccount,
	}, logr.Discard())
	if !errors.Is(err, errReconcileDone) {
		t.Fatalf("expected EKSPodIdentity config to finish reconcile without error, got target=%v result=%#v err=%v", target, result, err)
	}
	if target != nil || !result.IsZero() {
		t.Fatalf("expected no target and empty result, got target=%v result=%#v", target, result)
	}
}

func TestResolveAnnotationBasedIRSATargetWaitingInventoryUsesDeliveryType(t *testing.T) {
	ctx := context.Background()
	config := testEKSIRSAConfig(
		identityv1.OIDCProviderManagementExternal,
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	)
	localClient := testConfigClient(t, config)
	beforeEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonWaitingInventory)
	beforeSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonWaitingInventory)

	target, result, err := resolveAnnotationBasedIRSATarget(ctx, &selfHostedTargetRequest{
		LocalClient: localClient,
		Resolver:    inventory.Resolver{Client: localClient},
		MCManager:   &testRemoteClusterGetter{client: fakeClient(t)},
		ClusterName: multicluster.ClusterName(testResolvedClusterName),
		Namespace:   config.Namespace,
		Resource:    metrics.ResourceServiceAccount,
	}, logr.Discard())
	if !errors.Is(err, errReconcileDone) {
		t.Fatalf("expected waiting inventory to finish reconcile without error, got target=%v result=%#v err=%v", target, result, err)
	}
	if target != nil {
		t.Fatalf("expected no target while waiting for inventory, got %v", target)
	}
	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected fixed retry %s, got %#v", transientRequeue, result)
	}

	afterEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonWaitingInventory)
	afterSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonWaitingInventory)
	if got := afterEKSIRSA - beforeEKSIRSA; got != 1 {
		t.Fatalf("expected EKSIRSA waiting-inventory metric delta 1, got %v", got)
	}
	if got := afterSelfHosted - beforeSelfHosted; got != 0 {
		t.Fatalf("expected SelfHostedIRSA waiting-inventory metric delta 0, got %v", got)
	}
}

func TestResolveAnnotationBasedIRSATargetSkipsStaleClusterEvent(t *testing.T) {
	ctx := context.Background()
	config := testEKSIRSAConfig(
		identityv1.OIDCProviderManagementExternal,
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	)
	localClient := testConfigClient(t, config, testResolvedClusterProfile(config.Namespace))
	// Resolver maps namespace=config.Namespace -> ClusterName "<ns>/<ns>".
	// Passing a different req.ClusterName simulates a stale event left over from
	// a prior inventory resolution: the guard must skip without calling MCManager.
	staleClusterName := multicluster.ClusterName(testSiblingNamespace + "/" + testSiblingNamespace)
	if string(staleClusterName) == testResolvedClusterName {
		t.Fatalf("test precondition: stale cluster name must differ from resolved cluster name")
	}

	getter := &testRemoteClusterGetter{err: errors.New("GetCluster must not be called for stale event")}
	before := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.ResourceServiceAccount,
		metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonStaleClusterEvent)

	target, result, err := resolveAnnotationBasedIRSATarget(ctx, &selfHostedTargetRequest{
		LocalClient: localClient,
		Resolver:    inventory.Resolver{Client: localClient},
		MCManager:   getter,
		ClusterName: staleClusterName,
		Namespace:   config.Namespace,
		Resource:    metrics.ResourceServiceAccount,
	}, logr.Discard())
	if !errors.Is(err, errReconcileDone) {
		t.Fatalf("expected stale cluster event to finish reconcile without error, got target=%v result=%#v err=%v", target, result, err)
	}
	if target != nil {
		t.Fatalf("expected no target for stale cluster event, got %v", target)
	}
	if !result.IsZero() {
		t.Fatalf("expected empty result (no requeue) for stale cluster event, got %#v", result)
	}

	after := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.ResourceServiceAccount,
		metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonStaleClusterEvent)
	if got := after - before; got != 1 {
		t.Fatalf("expected stale_cluster_event metric delta 1, got %v", got)
	}
}

func TestResolveAnnotationBasedIRSATargetClusterUnavailableUsesFixedRetry(t *testing.T) {
	ctx := context.Background()
	clusterErr := errors.New("cluster unavailable")
	config := testEKSIRSAConfig(
		identityv1.OIDCProviderManagementExternal,
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	)
	localClient := testConfigClient(t, config, testResolvedClusterProfile(config.Namespace))
	beforeEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultError, metrics.RemoteDeliveryReasonClusterUnavail)
	beforeSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultError, metrics.RemoteDeliveryReasonClusterUnavail)

	target, result, err := resolveAnnotationBasedIRSATarget(ctx, &selfHostedTargetRequest{
		LocalClient: localClient,
		Resolver:    inventory.Resolver{Client: localClient},
		MCManager:   &testRemoteClusterGetter{err: clusterErr},
		ClusterName: multicluster.ClusterName(testResolvedClusterName),
		Namespace:   config.Namespace,
		Resource:    metrics.ResourceServiceAccount,
	}, logr.Discard())
	if !errors.Is(err, errReconcileDone) {
		t.Fatalf("expected cluster lookup failure to finish reconcile without bubbling original error, got target=%v result=%#v err=%v", target, result, err)
	}
	if target != nil {
		t.Fatalf("expected no target cluster on lookup failure, got %v", target)
	}
	if result.RequeueAfter != transientRequeue {
		t.Fatalf("expected fixed retry %s, got %#v", transientRequeue, result)
	}

	afterEKSIRSA := remoteDeliveryCount(t, identityv1.DeliveryTypeEKSIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultError, metrics.RemoteDeliveryReasonClusterUnavail)
	afterSelfHosted := remoteDeliveryCount(t, identityv1.DeliveryTypeSelfHostedIRSA, metrics.ResourceServiceAccount, metrics.RemoteDeliveryResultError, metrics.RemoteDeliveryReasonClusterUnavail)
	if got := afterEKSIRSA - beforeEKSIRSA; got != 1 {
		t.Fatalf("expected EKSIRSA cluster-unavailable metric delta 1, got %v", got)
	}
	if got := afterSelfHosted - beforeSelfHosted; got != 0 {
		t.Fatalf("expected SelfHostedIRSA cluster-unavailable metric delta 0, got %v", got)
	}
}

func remoteDeliveryCount(t *testing.T, deliveryType identityv1.DeliveryType, resource, result, reason string) float64 {
	t.Helper()

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != "awio_remote_delivery_total" {
			continue
		}

		for _, metric := range family.GetMetric() {
			labels := metric.GetLabel()
			if labelValue(labels, "delivery_type") != string(deliveryType) ||
				labelValue(labels, "resource") != resource ||
				labelValue(labels, "result") != result ||
				labelValue(labels, "reason") != reason {
				continue
			}

			return metric.GetCounter().GetValue()
		}
	}

	return 0
}
