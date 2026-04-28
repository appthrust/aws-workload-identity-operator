package managedclusterlabels

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/appthrust/aws-workload-identity-operator/internal/inventory"
)

func TestReconcileCopiesAndRemovesManagedLabels(t *testing.T) {
	ctx := context.Background()
	mc := testManagedCluster("wlc-a", map[string]string{
		"pod-cidr.appthrust.io":            "10.0.0.0/16",
		"source.appthrust.io/cluster-name": "wlc-a",
		"example.com/not-owned":            "ignore",
	})
	profile := testClusterProfile("inventory", "wlc-a", map[string]string{
		inventory.LabelOCMClusterName:      "wlc-a",
		"pod-cidr.appthrust.io":            "old",
		"stale.appthrust.io":               "remove",
		"source.appthrust.io/cluster-name": "old",
		"source.appthrust.io/stale":        "remove",
		"open-cluster-management.io/keep":  "preserve",
	})
	c := testClient(mc, profile)
	r := &Reconciler{Client: c}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: "wlc-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := newClusterProfile()
	if err := c.Get(ctx, client.ObjectKey{Namespace: "inventory", Name: "wlc-a"}, got); err != nil {
		t.Fatalf("get ClusterProfile: %v", err)
	}

	labels := got.GetLabels()

	if labels["pod-cidr.appthrust.io"] != "10.0.0.0/16" {
		t.Fatalf("label not copied: %#v", labels)
	}

	if labels["source.appthrust.io/cluster-name"] != "wlc-a" {
		t.Fatalf("source label not copied: %#v", labels)
	}

	if _, ok := labels["stale.appthrust.io"]; ok {
		t.Fatalf("stale appthrust label was not removed: %#v", labels)
	}

	if _, ok := labels["source.appthrust.io/stale"]; ok {
		t.Fatalf("stale source label was not removed: %#v", labels)
	}

	if labels["open-cluster-management.io/keep"] != "preserve" {
		t.Fatalf("OCM label was not preserved: %#v", labels)
	}

	if _, ok := labels["example.com/not-owned"]; ok {
		t.Fatalf("non-owned ManagedCluster label was copied: %#v", labels)
	}
}

func TestReconcileDoesNotMutateClusterProfileStatus(t *testing.T) {
	ctx := context.Background()
	mc := testManagedCluster("wlc-a", map[string]string{"pod-cidr.appthrust.io": "10.0.0.0/16"})
	profile := testClusterProfile("inventory", "wlc-a", map[string]string{inventory.LabelOCMClusterName: "wlc-a"})
	_ = unstructured.SetNestedField(profile.Object, "ready", "status", "phase")
	c := testClient(mc, profile)
	r := &Reconciler{Client: c}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: "wlc-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := newClusterProfile()
	if err := c.Get(ctx, client.ObjectKey{Namespace: "inventory", Name: "wlc-a"}, got); err != nil {
		t.Fatalf("get ClusterProfile: %v", err)
	}

	if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != "ready" {
		t.Fatalf("status mutated or lost, phase=%q object=%#v", phase, got.Object["status"])
	}
}

func TestReconcileUpdatesMultipleMatchingClusterProfiles(t *testing.T) {
	ctx := context.Background()
	mc := testManagedCluster("wlc-a", map[string]string{"source.appthrust.io/cluster-namespace": "work"})
	first := testClusterProfile("inventory-a", "wlc-a", map[string]string{inventory.LabelOCMClusterName: "wlc-a"})
	second := testClusterProfile("inventory-b", "wlc-a", map[string]string{inventory.LabelOCMClusterName: "wlc-a"})
	other := testClusterProfile("inventory-c", "other", map[string]string{inventory.LabelOCMClusterName: "other"})
	c := testClient(mc, first, second, other)
	r := &Reconciler{Client: c}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: "wlc-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, key := range []client.ObjectKey{{Namespace: "inventory-a", Name: "wlc-a"}, {Namespace: "inventory-b", Name: "wlc-a"}} {
		got := newClusterProfile()
		if err := c.Get(ctx, key, got); err != nil {
			t.Fatalf("get ClusterProfile %s: %v", key, err)
		}

		if got.GetLabels()["source.appthrust.io/cluster-namespace"] != "work" {
			t.Fatalf("ClusterProfile %s was not updated: %#v", key, got.GetLabels())
		}
	}

	gotOther := newClusterProfile()

	if err := c.Get(ctx, client.ObjectKey{Namespace: "inventory-c", Name: "other"}, gotOther); err != nil {
		t.Fatalf("get other ClusterProfile: %v", err)
	}

	if _, ok := gotOther.GetLabels()["source.appthrust.io/cluster-namespace"]; ok {
		t.Fatalf("unmatched ClusterProfile was updated: %#v", gotOther.GetLabels())
	}
}

func testManagedCluster(name string, labels map[string]string) *unstructured.Unstructured {
	mc := newManagedCluster(name)
	mc.SetLabels(labels)

	return mc
}

func testClusterProfile(namespace, name string, labels map[string]string) *unstructured.Unstructured {
	profile := newClusterProfile()
	profile.SetNamespace(namespace)
	profile.SetName(name)
	profile.SetLabels(labels)

	return profile
}

func testClient(objects ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(managedClusterGVK.GroupVersion(), newManagedCluster(""), &unstructured.UnstructuredList{})
	scheme.AddKnownTypes(clusterProfileGVK.GroupVersion(), newClusterProfile(), newClusterProfileList())

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

type recordedEvent struct {
	regarding runtime.Object
	related   runtime.Object
	eventType string
	reason    string
	action    string
	note      string
}

type capturingEventRecorder struct {
	events []recordedEvent
}

var _ events.EventRecorder = (*capturingEventRecorder)(nil)

func (r *capturingEventRecorder) Eventf(regarding, related runtime.Object, eventType, reason, action, note string, args ...interface{}) {
	r.events = append(r.events, recordedEvent{
		regarding: regarding,
		related:   related,
		eventType: eventType,
		reason:    reason,
		action:    action,
		note:      fmt.Sprintf(note, args...),
	})
}

func TestReconcileEmitsEventOnClusterProfileLabelUpdate(t *testing.T) {
	ctx := context.Background()
	mc := testManagedCluster("wlc-a", map[string]string{
		"pod-cidr.appthrust.io":            "10.0.0.0/16",
		"source.appthrust.io/cluster-name": "wlc-a",
	})
	profile := testClusterProfile("inventory", "wlc-a", map[string]string{
		inventory.LabelOCMClusterName: "wlc-a",
	})
	c := testClient(mc, profile)
	recorder := &capturingEventRecorder{}
	r := &Reconciler{Client: c, Recorder: recorder}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: "wlc-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(recorder.events) != 1 {
		t.Fatalf("expected exactly one event, got %d: %#v", len(recorder.events), recorder.events)
	}

	event := recorder.events[0]

	regarding, ok := event.regarding.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("expected regarding to be ClusterProfile *unstructured.Unstructured, got %T", event.regarding)
	}

	if regarding.GroupVersionKind() != clusterProfileGVK {
		t.Fatalf("expected regarding GVK %v, got %v", clusterProfileGVK, regarding.GroupVersionKind())
	}

	if regarding.GetNamespace() != "inventory" || regarding.GetName() != "wlc-a" {
		t.Fatalf("expected regarding ClusterProfile inventory/wlc-a, got %s/%s", regarding.GetNamespace(), regarding.GetName())
	}

	if event.related != nil {
		t.Fatalf("expected no related object, got %#v", event.related)
	}

	if event.eventType != corev1.EventTypeNormal {
		t.Fatalf("expected eventType %q, got %q", corev1.EventTypeNormal, event.eventType)
	}

	if event.reason != eventReasonClusterProfileLabelsMirrored {
		t.Fatalf("expected reason %q, got %q", eventReasonClusterProfileLabelsMirrored, event.reason)
	}

	if event.action != eventActionMirrorLabels {
		t.Fatalf("expected action %q, got %q", eventActionMirrorLabels, event.action)
	}

	if !strings.Contains(event.note, "wlc-a") {
		t.Fatalf("expected note to mention source ManagedCluster %q, got %q", "wlc-a", event.note)
	}
}

func TestReconcileNoEventWhenLabelsAlreadyUpToDate(t *testing.T) {
	ctx := context.Background()
	mc := testManagedCluster("wlc-a", map[string]string{
		"pod-cidr.appthrust.io":            "10.0.0.0/16",
		"source.appthrust.io/cluster-name": "wlc-a",
	})
	profile := testClusterProfile("inventory", "wlc-a", map[string]string{
		inventory.LabelOCMClusterName:      "wlc-a",
		"pod-cidr.appthrust.io":            "10.0.0.0/16",
		"source.appthrust.io/cluster-name": "wlc-a",
	})
	c := testClient(mc, profile)
	recorder := &capturingEventRecorder{}
	r := &Reconciler{Client: c, Recorder: recorder}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: "wlc-a"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(recorder.events) != 0 {
		t.Fatalf("expected no events when labels already match, got %d: %#v", len(recorder.events), recorder.events)
	}
}

func TestReconcileNilRecorderDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	mc := testManagedCluster("wlc-nil", map[string]string{
		"pod-cidr.appthrust.io": "10.0.0.0/16",
	})
	profile := testClusterProfile("inventory", "wlc-nil", map[string]string{
		inventory.LabelOCMClusterName: "wlc-nil",
	})
	c := testClient(mc, profile)
	r := &Reconciler{Client: c}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: "wlc-nil"}}); err != nil {
		t.Fatalf("reconcile with nil Recorder: %v", err)
	}

	got := newClusterProfile()
	if err := c.Get(ctx, client.ObjectKey{Namespace: "inventory", Name: "wlc-nil"}, got); err != nil {
		t.Fatalf("get ClusterProfile: %v", err)
	}

	if got.GetLabels()["pod-cidr.appthrust.io"] != "10.0.0.0/16" {
		t.Fatalf("expected label to be patched even with nil Recorder, got %#v", got.GetLabels())
	}
}
