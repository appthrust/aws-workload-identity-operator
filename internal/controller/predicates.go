package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/appthrust/aws-workload-identity-operator/internal/observability/metrics"
)

// predicateKeepFns selects events for metricsRecordingPredicate.
// A nil field means "drop".
type predicateKeepFns struct {
	Create  func(event.CreateEvent) bool
	Update  func(event.UpdateEvent) bool
	Delete  func(event.DeleteEvent) bool
	Generic func(event.GenericEvent) bool
}

func metricsRecordingPredicate(controllerName string, fns predicateKeepFns) predicate.Funcs {
	record := func(keep bool) bool {
		decision := metrics.PredicateDecisionDropped
		if keep {
			decision = metrics.PredicateDecisionKept
		}

		metrics.RecordPredicateDecision(controllerName, decision)

		return keep
	}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return record(fns.Create != nil && fns.Create(e))
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return record(fns.Update != nil && fns.Update(e))
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return record(fns.Delete != nil && fns.Delete(e))
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return record(fns.Generic != nil && fns.Generic(e))
		},
	}
}

func rootObjectChangedPredicate(controllerName string) predicate.Predicate {
	return metricsRecordingPredicate(controllerName, predicateKeepFns{
		Create:  func(_ event.CreateEvent) bool { return true },
		Update:  rootObjectUpdateChanged,
		Delete:  func(_ event.DeleteEvent) bool { return true },
		Generic: func(_ event.GenericEvent) bool { return true },
	})
}

func rootObjectUpdateChanged(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}

	return predicate.GenerationChangedPredicate{}.Update(e) ||
		predicate.AnnotationChangedPredicate{}.Update(e) ||
		e.ObjectNew.GetDeletionTimestamp().IsZero() != e.ObjectOld.GetDeletionTimestamp().IsZero()
}
