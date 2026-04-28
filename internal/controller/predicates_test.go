package controller

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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
			name: "finalizer added",
			buildOld: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Finalizers = nil

				return obj
			},
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Finalizers = []string{identityv1.ConfigFinalizer}

				return obj
			},
			want: true,
		},
		{
			name: "finalizer removed",
			buildOld: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Finalizers = []string{identityv1.ConfigFinalizer}

				return obj
			},
			buildNew: func() *identityv1.AWSWorkloadIdentityConfig {
				obj := baseConfig()
				obj.Finalizers = nil

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

// crdWithEstablished returns a CustomResourceDefinition with the given name
// and an Established condition status. Pass an empty status string to omit
// the Established condition entirely (covers "no condition at all" cases).
func crdWithEstablished(name string, status apiextensionsv1.ConditionStatus) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}

	if status != "" {
		crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.Established, Status: status},
		}
	}

	return crd
}

// TestAckChildCRDChangedPredicate asserts the predicate fires the parent
// reconcile only when the target optional ACK CRD reaches Established=True,
// so a CRD installed after operator startup still triggers the initial
// reconcile without flooding the queue with unrelated cluster-wide CRD
// events.
//
//nolint:funlen // table-driven predicate cases kept inline; extracting them obscures the per-case event/want pairing.
func TestAckChildCRDChangedPredicate(t *testing.T) {
	const (
		targetCRD = "buckets.s3.services.k8s.aws"
		otherCRD  = "other.example.com"
	)

	tests := []struct {
		name       string
		create     *apiextensionsv1.CustomResourceDefinition
		updateOld  *apiextensionsv1.CustomResourceDefinition
		updateNew  *apiextensionsv1.CustomResourceDefinition
		deleteObj  *apiextensionsv1.CustomResourceDefinition
		wantCreate bool
		wantUpdate bool
		wantDelete bool
	}{
		{
			name:      "non-matching CRD name drops all events",
			create:    crdWithEstablished(otherCRD, apiextensionsv1.ConditionTrue),
			updateOld: crdWithEstablished(otherCRD, apiextensionsv1.ConditionFalse),
			updateNew: crdWithEstablished(otherCRD, apiextensionsv1.ConditionTrue),
			deleteObj: crdWithEstablished(otherCRD, apiextensionsv1.ConditionTrue),
		},
		{
			name:       "create with Established=True is kept",
			create:     crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue),
			wantCreate: true,
		},
		{
			name:   "create with Established=False is dropped",
			create: crdWithEstablished(targetCRD, apiextensionsv1.ConditionFalse),
		},
		{
			name:       "update transitioning Established False to True is kept",
			updateOld:  crdWithEstablished(targetCRD, apiextensionsv1.ConditionFalse),
			updateNew:  crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue),
			wantUpdate: true,
		},
		{
			name:      "update with steady-state Established=True is dropped",
			updateOld: crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue),
			updateNew: crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue),
		},
		{
			name:      "update transitioning Established True to False is dropped",
			updateOld: crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue),
			updateNew: crdWithEstablished(targetCRD, apiextensionsv1.ConditionFalse),
		},
		{
			name:      "update without Established condition on either side is dropped",
			updateOld: crdWithEstablished(targetCRD, ""),
			updateNew: crdWithEstablished(targetCRD, ""),
		},
		{
			name:      "delete with matching name and Established=True is dropped",
			deleteObj: crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pred := ackChildCRDChangedPredicate(metrics.ControllerConfig, targetCRD)
			assertAckChildCRDCreate(t, pred, tc.create, tc.wantCreate)
			assertAckChildCRDUpdate(t, pred, tc.updateOld, tc.updateNew, tc.wantUpdate)
			assertAckChildCRDDelete(t, pred, tc.deleteObj, tc.wantDelete)
		})
	}
}

func assertAckChildCRDCreate(t *testing.T, pred predicate.Predicate, obj *apiextensionsv1.CustomResourceDefinition, want bool) {
	t.Helper()

	if obj == nil {
		return
	}

	if got := pred.Create(event.CreateEvent{Object: obj}); got != want {
		t.Fatalf("Create() = %v, want %v", got, want)
	}
}

func assertAckChildCRDUpdate(t *testing.T, pred predicate.Predicate, oldObj, newObj *apiextensionsv1.CustomResourceDefinition, want bool) {
	t.Helper()

	if oldObj == nil && newObj == nil {
		return
	}

	ev := event.UpdateEvent{}
	if oldObj != nil {
		ev.ObjectOld = oldObj
	}

	if newObj != nil {
		ev.ObjectNew = newObj
	}

	if got := pred.Update(ev); got != want {
		t.Fatalf("Update() = %v, want %v", got, want)
	}
}

func assertAckChildCRDDelete(t *testing.T, pred predicate.Predicate, obj *apiextensionsv1.CustomResourceDefinition, want bool) {
	t.Helper()

	if obj == nil {
		return
	}

	if got := pred.Delete(event.DeleteEvent{Object: obj}); got != want {
		t.Fatalf("Delete() = %v, want %v", got, want)
	}
}

// TestAckChildCRDChangedPredicateDropsGenericAndNilUpdate asserts the
// predicate is closed against generic events and partial update events
// (where one side is nil), matching the "no inverse trigger" contract.
func TestAckChildCRDChangedPredicateDropsGenericAndNilUpdate(t *testing.T) {
	const targetCRD = "buckets.s3.services.k8s.aws"

	pred := ackChildCRDChangedPredicate(metrics.ControllerConfig, targetCRD)

	if pred.Generic(event.GenericEvent{Object: crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue)}) {
		t.Fatal("expected Generic event to be dropped")
	}

	if pred.Update(event.UpdateEvent{ObjectNew: crdWithEstablished(targetCRD, apiextensionsv1.ConditionTrue)}) {
		t.Fatal("expected Update event with nil ObjectOld to be dropped")
	}

	if pred.Update(event.UpdateEvent{ObjectOld: crdWithEstablished(targetCRD, apiextensionsv1.ConditionFalse)}) {
		t.Fatal("expected Update event with nil ObjectNew to be dropped")
	}
}
