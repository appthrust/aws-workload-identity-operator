package controller

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

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

func TestRecordFinalizerEvents(t *testing.T) {
	recorder := &capturingEventRecorder{}
	obj := &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{Name: identityv1.DefaultName, Namespace: testInventoryNamespace},
	}

	recordFinalizerAdded(recorder, obj)
	recordFinalizerRemoved(recorder, obj)

	expected := []recordedEvent{
		{
			regarding: obj,
			eventType: corev1.EventTypeNormal,
			reason:    eventReasonFinalizerAdded,
			action:    eventActionAddFinalizer,
			note:      "added finalizer for cleanup",
		},
		{
			regarding: obj,
			eventType: corev1.EventTypeNormal,
			reason:    eventReasonFinalizerRemoved,
			action:    eventActionRemoveFinalizer,
			note:      "removed finalizer after cleanup",
		},
	}

	assertRecordedEvents(t, recorder.events, expected)
}

func TestRecordConditionEvents(t *testing.T) {
	recorder := &capturingEventRecorder{}
	obj := &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{Name: identityv1.DefaultName, Namespace: testInventoryNamespace},
	}

	recordConditionEvents(recorder, obj, []metav1.Condition{
		{
			Type:   identityv1.ConditionReady,
			Status: metav1.ConditionTrue,
			Reason: identityv1.ReasonReady,
		},
		{
			Type:   identityv1.ConditionOperatorConfigReady,
			Status: metav1.ConditionFalse,
			Reason: identityv1.ReasonInvalidSpec,
		},
		{
			Type:   identityv1.ConditionOperatorConfigReady,
			Status: metav1.ConditionTrue,
			Reason: identityv1.ReasonReady,
		},
	})

	expected := []recordedEvent{
		{
			regarding: obj,
			eventType: corev1.EventTypeNormal,
			reason:    identityv1.ReasonReady,
			action:    eventActionConditionTransitioned,
			note:      "Ready transitioned to True with reason Ready",
		},
		{
			regarding: obj,
			eventType: corev1.EventTypeWarning,
			reason:    identityv1.ReasonInvalidSpec,
			action:    eventActionConditionTransitioned,
			note:      "OperatorConfigReady transitioned to False with reason InvalidSpec",
		},
	}

	assertRecordedEvents(t, recorder.events, expected)
}

func assertRecordedEvents(t *testing.T, actual, expected []recordedEvent) {
	t.Helper()

	if len(actual) != len(expected) {
		t.Fatalf("expected %d events, got %d: %#v", len(expected), len(actual), actual)
	}

	for i := range expected {
		if actual[i].regarding != expected[i].regarding {
			t.Fatalf("event %d regarding object mismatch", i)
		}

		if actual[i].related != nil {
			t.Fatalf("event %d expected no related object, got %#v", i, actual[i].related)
		}

		if actual[i].eventType != expected[i].eventType ||
			actual[i].reason != expected[i].reason ||
			actual[i].action != expected[i].action ||
			actual[i].note != expected[i].note {
			t.Fatalf("event %d mismatch\nexpected: %#v\nactual:   %#v", i, expected[i], actual[i])
		}
	}
}
