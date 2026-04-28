package controller

import (
	"context"
	"fmt"

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
	nn, err := namespacedNameFromString(clusterName)
	if err != nil {
		return ""
	}

	return nn.Namespace
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

// remoteClusterReader resolves the workload-cluster uncached APIReader for the
// given inventory resolution. Used by ownership-verification Gets that must
// bypass selector-scoped caches (see deleteManagedRemoteWebhookRuntimeObject).
func remoteClusterReader(ctx context.Context, getter remoteClusterGetter, resolved *inventory.Resolution) (client.Reader, error) {
	if getter == nil {
		return nil, fmt.Errorf("multicluster manager is not configured")
	}

	clusterName := multicluster.ClusterName(resolved.ClusterName.String())

	target, err := getter.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("get remote cluster %s: %w", clusterName, err)
	}

	return target.GetAPIReader(), nil
}

type selfHostedTargetRequest struct {
	LocalClient client.Reader
	Resolver    inventory.Resolver
	MCManager   remoteClusterGetter
	ClusterName multicluster.ClusterName
	Namespace   string
	Resource    string
}

type annotationBasedIRSATarget struct {
	Cluster      cluster.Cluster
	DeliveryType identityv1.DeliveryType
}

// loadSelfHostedConfig fetches AWSWorkloadIdentityConfig/default for namespace
// and verifies it is configured for SelfHostedIRSA. Returns errReconcileDone
// when the caller should stop without an error (config missing or wrong
// delivery type); a real error wraps the API failure.
func loadSelfHostedConfig(ctx context.Context, reader client.Reader, namespace, resource string, log logr.Logger) (*identityv1.AWSWorkloadIdentityConfig, error) {
	return loadConfigMatching(ctx, reader, namespace, resource,
		string(identityv1.DeliveryTypeSelfHostedIRSA),
		func(t identityv1.DeliveryType) bool { return t == identityv1.DeliveryTypeSelfHostedIRSA },
		"skipping non-self-hosted config",
		log)
}

func loadAnnotationBasedIRSAConfig(ctx context.Context, reader client.Reader, namespace, resource string, log logr.Logger) (*identityv1.AWSWorkloadIdentityConfig, error) {
	return loadConfigMatching(ctx, reader, namespace, resource,
		"",
		identityv1.DeliveryType.UsesAnnotationBasedIRSA,
		"skipping non-annotation-based IRSA config",
		log)
}

func loadConfigMatching(
	ctx context.Context,
	reader client.Reader,
	namespace, resource, notFoundDeliveryLabel string,
	accept func(identityv1.DeliveryType) bool,
	skipMessage string,
	log logr.Logger,
) (*identityv1.AWSWorkloadIdentityConfig, error) {
	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identityv1.DefaultName}, config); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.RecordRemoteDelivery(notFoundDeliveryLabel, resource, metrics.RemoteDeliveryResultSkipped, identityv1.ReasonConfigUnavailable)

			return nil, errReconcileDone
		}

		return nil, fmt.Errorf("get AWSWorkloadIdentityConfig/default in namespace %q: %w", namespace, err)
	}

	if !accept(config.Spec.Type) {
		metrics.RecordRemoteDelivery(string(config.Spec.Type), resource, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonNotSelfHosted)
		log.V(1).Info(skipMessage, logKeyDeliveryType, string(config.Spec.Type))

		return nil, errReconcileDone
	}

	return config, nil
}

func resolveAnnotationBasedIRSATarget(ctx context.Context, req *selfHostedTargetRequest, log logr.Logger) (*annotationBasedIRSATarget, ctrl.Result, error) {
	config, err := loadAnnotationBasedIRSAConfig(ctx, req.LocalClient, req.Namespace, req.Resource, log)
	if err != nil {
		return nil, ctrl.Result{}, err
	}

	resolved, err := req.Resolver.Resolve(ctx, req.Namespace)
	if err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("resolve inventory namespace %q: %w", req.Namespace, err)
	}

	if !resolved.Ready {
		metrics.RecordRemoteDelivery(string(config.Spec.Type), req.Resource, metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonWaitingInventory)
		log.V(1).Info("waiting for inventory resolution",
			logKeyDeliveryType, string(config.Spec.Type),
			logKeyConditionReason, resolved.Reason,
		)

		return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	if multicluster.ClusterName(resolved.ClusterName.String()) != req.ClusterName {
		metrics.RecordRemoteDelivery(string(config.Spec.Type), req.Resource,
			metrics.RemoteDeliveryResultSkipped, metrics.RemoteDeliveryReasonStaleClusterEvent)
		log.V(1).Info("dropping stale remote event; inventory now resolves to a different cluster",
			logKeyResolvedClusterName, resolved.ClusterName.String())

		return nil, ctrl.Result{}, errReconcileDone
	}

	target, err := req.MCManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		metrics.RecordRemoteDelivery(string(config.Spec.Type), req.Resource, metrics.RemoteDeliveryResultError, metrics.RemoteDeliveryReasonClusterUnavail)
		log.Error(err, "annotation-based IRSA remote delivery deferred",
			logKeyDeliveryType, string(config.Spec.Type),
			logKeyConditionReason, metrics.RemoteDeliveryReasonClusterUnavail,
		)

		return nil, ctrl.Result{RequeueAfter: transientRequeue}, errReconcileDone
	}

	return &annotationBasedIRSATarget{
		Cluster:      target,
		DeliveryType: config.Spec.Type,
	}, ctrl.Result{}, nil
}
