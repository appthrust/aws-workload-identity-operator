package controller

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

const testStatusOnlyIssuerHostPath = "issuer.example.com/foo"

//nolint:funlen // table-driven predicate cases kept inline; extracting them obscures the per-case mutate/want pairing.
func TestRootObjectUpdateChanged(t *testing.T) {
	now := metav1.NewTime(time.Now())

	baseConfig := func() *identityv1.AWSWorkloadIdentityConfig {
		return &identityv1.AWSWorkloadIdentityConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:       identityv1.DefaultName,
				Namespace:  testInventoryNamespace,
				Generation: 3,
				Annotations: map[string]string{
					"example.com/keep": "value",
				},
			},
			Spec: identityv1.AWSWorkloadIdentityConfigSpec{
				Type:   identityv1.DeliveryTypeSelfHostedIRSA,
				Region: "us-east-1",
			},
		}
	}

	tests := []struct {
		name     string
		buildOld func() *identityv1.AWSWorkloadIdentityConfig
		buildNew func() *identityv1.AWSWorkloadIdentityConfig
		oldNil   bool
		newNil   bool
		want     bool
	}{
		{
			name:     "ObjectOld is nil",
			buildNew: baseConfig,
			oldNil:   true,
			want:     false,
		},
		{
			name:     "ObjectNew is nil",
			buildOld: baseConfig,
			newNil:   true,
			want:     false,
		},
		{
			name:     "generation increased",
			buildOld: baseConfig,
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Generation = 4

				return obj
			},
			want: true,
		},
		{
			name:     "deletion timestamp transitioned from zero to set",
			buildOld: baseConfig,
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				ts := now
				obj.DeletionTimestamp = &ts

				return obj
			},
			want: true,
		},
		{
			name: "deletion timestamp transitioned from set to zero",
			buildOld: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				ts := now
				obj.DeletionTimestamp = &ts

				return obj
			},
			buildNew: baseConfig,
			want:     true,
		},
		{
			name: "annotation added",
			buildOld: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Annotations = map[string]string{}

				return obj
			},
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Annotations = map[string]string{
					identityv1.ForceDeleteAnnotation: "true",
				}

				return obj
			},
			want: true,
		},
		{
			name: "annotation removed",
			buildOld: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Annotations = map[string]string{
					identityv1.ForceDeleteAnnotation: "true",
				}

				return obj
			},
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Annotations = map[string]string{}

				return obj
			},
			want: true,
		},
		{
			name: "annotations swapped to different keys",
			buildOld: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Annotations = map[string]string{"a": "1"}

				return obj
			},
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Annotations = map[string]string{"b": "1"}

				return obj
			},
			want: true,
		},
		{
			name:     "status-only change keeps generation, deletion, and annotations equal",
			buildOld: baseConfig,
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Status.ObservedGeneration = obj.Generation
				obj.Status.Conditions = []metav1.Condition{
					{
						Type:               identityv1.ConditionReady,
						Status:             metav1.ConditionTrue,
						Reason:             identityv1.ReasonReady,
						LastTransitionTime: now,
					},
				}
				obj.Status.IssuerHostPath = testStatusOnlyIssuerHostPath

				return obj
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := event.UpdateEvent{}

			if !tc.oldNil && tc.buildOld != nil {
				ev.ObjectOld = tc.buildOld()
			}

			if !tc.newNil && tc.buildNew != nil {
				ev.ObjectNew = tc.buildNew()
			}

			if got := rootObjectUpdateChanged(ev); got != tc.want {
				t.Fatalf("rootObjectUpdateChanged() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRootObjectChangedPredicate(t *testing.T) {
	pred := rootObjectChangedPredicate(metrics.ControllerConfig)

	obj := &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityv1.DefaultName,
			Namespace: testInventoryNamespace,
		},
	}

	if !pred.Create(event.CreateEvent{Object: obj}) {
		t.Fatal("expected Create event to be kept")
	}

	if !pred.Delete(event.DeleteEvent{Object: obj}) {
		t.Fatal("expected Delete event to be kept")
	}

	if !pred.Generic(event.GenericEvent{Object: obj}) {
		t.Fatal("expected Generic event to be kept")
	}

	statusOnlyOld := obj.DeepCopy()
	statusOnlyNew := obj.DeepCopy()
	statusOnlyNew.Status.IssuerHostPath = testStatusOnlyIssuerHostPath

	if pred.Update(event.UpdateEvent{ObjectOld: statusOnlyOld, ObjectNew: statusOnlyNew}) {
		t.Fatal("expected status-only Update event to be dropped")
	}

	specChangedOld := obj.DeepCopy()
	specChangedOld.Generation = 1
	specChangedNew := specChangedOld.DeepCopy()
	specChangedNew.Generation = 2

	if !pred.Update(event.UpdateEvent{ObjectOld: specChangedOld, ObjectNew: specChangedNew}) {
		t.Fatal("expected spec-changed Update event to be kept")
	}
}

// TestRootObjectChangedPredicateRecordsControllerLabel asserts each controller
// gets a distinct metric series so cross-controller decisions are never merged.
func TestRootObjectChangedPredicateRecordsControllerLabel(t *testing.T) {
	obj := &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identityv1.DefaultName,
			Namespace: testInventoryNamespace,
		},
	}

	statusOnlyOld := obj.DeepCopy()
	statusOnlyNew := obj.DeepCopy()
	statusOnlyNew.Status.IssuerHostPath = testStatusOnlyIssuerHostPath
	droppedEvent := event.UpdateEvent{ObjectOld: statusOnlyOld, ObjectNew: statusOnlyNew}

	before := predicateDroppedCounts(t)

	if rootObjectChangedPredicate(metrics.ControllerRole).Update(droppedEvent) {
		t.Fatal("expected status-only Update event to be dropped for ControllerRole")
	}

	if rootObjectChangedPredicate(metrics.ControllerConfig).Update(droppedEvent) {
		t.Fatal("expected status-only Update event to be dropped for ControllerConfig")
	}

	after := predicateDroppedCounts(t)

	for _, controller := range []string{metrics.ControllerRole, metrics.ControllerConfig} {
		if got := after[controller] - before[controller]; got != 1 {
			t.Fatalf("predicate dropped count for controller=%q delta=%v, want 1", controller, got)
		}
	}
}

func predicateDroppedCounts(t *testing.T) map[string]float64 {
	t.Helper()

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	counts := map[string]float64{}

	for _, family := range families {
		if family.GetName() != "awio_predicate_decision_total" {
			continue
		}

		for _, m := range family.GetMetric() {
			if labelValue(m.GetLabel(), "decision") != metrics.PredicateDecisionDropped {
				continue
			}

			controller := labelValue(m.GetLabel(), "controller")
			counts[controller] = m.GetCounter().GetValue()
		}
	}

	return counts
}

func labelValue(pairs []*dto.LabelPair, name string) string {
	for _, pair := range pairs {
		if pair.GetName() == name {
			return pair.GetValue()
		}
	}

	return ""
}
