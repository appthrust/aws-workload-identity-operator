package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

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

func TestResolveSelfHostedTargetClusterUnavailableUsesFixedRetry(t *testing.T) {
	ctx := context.Background()
	clusterErr := errors.New("cluster unavailable")
	config := testSelfHostedConfig()
	localClient := testConfigClient(t, config, testResolvedClusterProfile(config.Namespace))

	target, result, err := resolveSelfHostedTarget(ctx, &selfHostedTargetRequest{
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
}
