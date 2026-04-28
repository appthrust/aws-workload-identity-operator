// Package managedclusterlabels mirrors selected OCM ManagedCluster labels to
// Cluster Inventory ClusterProfiles.
package managedclusterlabels

import (
	"context"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
)

const (
	eventReasonClusterProfileLabelsMirrored = "ClusterProfileLabelsMirrored"
	eventActionMirrorLabels                 = "MirrorLabels"
)

var (
	managedClusterGVK = schema.GroupVersionKind{Group: "cluster.open-cluster-management.io", Version: "v1", Kind: "ManagedCluster"}
	clusterProfileGVK = schema.GroupVersionKind{Group: "multicluster.x-k8s.io", Version: "v1alpha1", Kind: "ClusterProfile"}
)

// Reconciler mirrors appthrust-owned ManagedCluster labels to matching ClusterProfiles.
type Reconciler struct {
	client.Client

	MaxConcurrentReconciles int

	// Recorder, when non-nil, receives Events on ClusterProfiles whose labels
	// were updated. Optional; the controller is functional without it.
	Recorder events.EventRecorder
}

// Reconcile mirrors appthrust-owned labels from one ManagedCluster to matching
// ClusterProfiles selected by the OCM cluster-name label.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues(
		"controller", "managedclusterlabels",
		"k8s.resource.group", managedClusterGVK.Group,
		"k8s.resource.kind", managedClusterGVK.Kind,
		"k8s.resource.name", req.Name,
	)
	ctx = logf.IntoContext(ctx, log)

	managedCluster := newManagedCluster(req.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, managedCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("get ManagedCluster %s: %w", req.Name, err)
	}

	profiles := newClusterProfileList()
	if err := r.List(ctx, profiles, client.MatchingLabels{inventory.LabelOCMClusterName: managedCluster.GetName()}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ClusterProfiles for ManagedCluster %s: %w", managedCluster.GetName(), err)
	}

	for i := range profiles.Items {
		profile := &profiles.Items[i]
		if err := r.reconcileProfile(ctx, managedCluster, profile); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileProfile(ctx context.Context, managedCluster, profile *unstructured.Unstructured) error {
	log := logf.FromContext(ctx).WithValues(
		"k8s.resource.group", clusterProfileGVK.Group,
		"k8s.resource.kind", clusterProfileGVK.Kind,
		"k8s.resource.namespace", profile.GetNamespace(),
		"k8s.resource.name", profile.GetName(),
	)

	desired := desiredLabels(managedCluster)
	current := profile.GetLabels()

	next := map[string]string{}
	maps.Copy(next, current)

	for key := range next {
		if managedLabel(key) {
			delete(next, key)
		}
	}

	maps.Copy(next, desired)

	if maps.Equal(current, next) {
		return nil
	}

	updated := profile.DeepCopy()
	updated.SetLabels(next)

	if err := r.Patch(ctx, updated, client.MergeFrom(profile)); err != nil {
		return fmt.Errorf("update ClusterProfile %s/%s labels: %w", profile.GetNamespace(), profile.GetName(), err)
	}

	log.Info("ClusterProfile labels updated")

	if r.Recorder != nil {
		r.Recorder.Eventf(updated, nil, corev1.EventTypeNormal, eventReasonClusterProfileLabelsMirrored, eventActionMirrorLabels,
			"mirrored appthrust-owned labels from ManagedCluster %q", managedCluster.GetName())
	}

	return nil
}

func desiredLabels(managedCluster client.Object) map[string]string {
	desired := map[string]string{}

	for key, value := range managedCluster.GetLabels() {
		if managedLabel(key) {
			desired[key] = value
		}
	}

	return desired
}

func managedLabel(key string) bool {
	return strings.HasPrefix(key, "source.appthrust.io/") || strings.HasSuffix(key, ".appthrust.io") || strings.Contains(key, ".appthrust.io/")
}

func newManagedCluster(name string) *unstructured.Unstructured {
	mc := &unstructured.Unstructured{}
	mc.SetGroupVersionKind(managedClusterGVK)
	mc.SetName(name)

	return mc
}

func newClusterProfile() *unstructured.Unstructured {
	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(clusterProfileGVK)

	return profile
}

func newClusterProfileList() *unstructured.UnstructuredList {
	profiles := &unstructured.UnstructuredList{}
	profiles.SetGroupVersionKind(clusterProfileGVK.GroupVersion().WithKind("ClusterProfileList"))

	return profiles
}

// SetupWithManager registers the reconciler with a controller manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only label changes can affect the desired ClusterProfile labels; status
	// heartbeats from ManagedCluster lease updates would otherwise re-enter
	// Reconcile (~every 60s per cluster) for no work.
	labelOnly := builder.WithPredicates(predicate.LabelChangedPredicate{})

	if err := ctrl.NewControllerManagedBy(mgr).
		For(newManagedCluster(""), labelOnly).
		Watches(newClusterProfile(), handler.EnqueueRequestsFromMapFunc(clusterForProfile), labelOnly).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r); err != nil {
		return fmt.Errorf("set up ManagedCluster label mirror controller: %w", err)
	}

	return nil
}

func clusterForProfile(_ context.Context, obj client.Object) []reconcile.Request {
	clusterName := obj.GetLabels()[inventory.LabelOCMClusterName]
	if clusterName == "" {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: clusterName}}}
}

var _ reconcile.Reconciler = (*Reconciler)(nil)
