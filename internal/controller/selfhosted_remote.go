package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

func inventoryNamespaceFromCluster(clusterName string) string {
	namespace, name, ok := strings.Cut(clusterName, "/")
	if !ok || namespace == "" || name == "" {
		return ""
	}

	return namespace
}

type remoteClusterGetter interface {
	GetCluster(context.Context, multicluster.ClusterName) (cluster.Cluster, error)
}

// remoteClusterClient resolves the workload-cluster client for the given
// inventory resolution. Callers map the returned error onto a condition or
// retry result; multicluster.ErrClusterNotFound is preserved for `errors.Is`.
func remoteClusterClient(ctx context.Context, getter remoteClusterGetter, resolved *inventory.Resolution) (client.Client, error) {
	if getter == nil {
		return nil, fmt.Errorf("multicluster manager is not configured")
	}

	clusterName := multicluster.ClusterName(resolved.ClusterName.String())

	target, err := getter.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("get remote cluster %s: %w", clusterName, err)
	}

	return target.GetClient(), nil
}

type selfHostedTargetRequest struct {
	LocalClient client.Reader
	Resolver    inventory.Resolver
	MCManager   remoteClusterGetter
	ClusterName multicluster.ClusterName
	Namespace   string
	Resource    string
}

// loadSelfHostedConfig fetches AWSWorkloadIdentityConfig/default for namespace
// and verifies it is configured for SelfHostedIRSA. Returns errReconcileDone
// when the caller should stop without an error (config missing or wrong
// delivery type); a real error wraps the API failure.
func loadSelfHostedConfig(ctx context.Context, reader client.Reader, namespace, resource string, log logr.Logger) (*identityv1.AWSWorkloadIdentityConfig, error) {
	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identityv1.DefaultName}, config); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), resource, metrics.RemoteDeliveryResultSkipped, identityv1.ReasonConfigUnavailable)

			return nil, errReconcileDone
		}

		return nil, fmt.Errorf("get AWSWorkloadIdentityConfig/default in namespace %q: %w", namespace, err)
	}

	if config.Spec.Type != identityv1.DeliveryTypeSelfHostedIRSA {
		metrics.RecordRemoteDelivery(string(config.Spec.Type), resource, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonNotSelfHosted)
		log.V(1).Info("skipping non-self-hosted config", "awio.delivery.type", string(config.Spec.Type))

		return nil, errReconcileDone
	}

	return config, nil
}

// resolveSelfHostedTarget shares the resolve -> config-fetch -> cluster-fetch
// flow between self-hosted remote controllers. The returned ctrl.Result tells
// the caller whether to requeue; a nil error means continue, errReconcileDone
// means the helper already produced a final result and the caller should
// return without further work.
func resolveSelfHostedTarget(ctx context.Context, req *selfHostedTargetRequest, log logr.Logger) (cluster.Cluster, ctrl.Result, error) {
	resolved, err := req.Resolver.Resolve(ctx, req.Namespace)
	if err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("resolve inventory namespace %q: %w", req.Namespace, err)
	}

	if !resolved.Ready {
		metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), req.Resource, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonWaitingInventory)
		log.V(1).Info("waiting for inventory resolution", "awio.condition.reason", resolved.Reason)

		return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	if _, err := loadSelfHostedConfig(ctx, req.LocalClient, req.Namespace, req.Resource, log); err != nil {
		return nil, ctrl.Result{}, err
	}

	target, err := req.MCManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		metrics.RecordRemoteDelivery(string(identityv1.DeliveryTypeSelfHostedIRSA), req.Resource, metrics.RemoteDeliveryResultError, metrics.RemoteDeliveryReasonClusterUnavail)
		log.Error(err, "self-hosted remote delivery deferred", "awio.condition.reason", metrics.RemoteDeliveryReasonClusterUnavail)

		return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	return target, ctrl.Result{}, nil
}
