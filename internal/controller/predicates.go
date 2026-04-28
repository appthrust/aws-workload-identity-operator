package controller

import (
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		!slices.Equal(e.ObjectNew.GetFinalizers(), e.ObjectOld.GetFinalizers()) ||
		e.ObjectNew.GetDeletionTimestamp().IsZero() != e.ObjectOld.GetDeletionTimestamp().IsZero()
}

// roleDeletedForConfigPredicate keeps only Delete events so the parent
// AWSWorkloadIdentityConfig reconcile wakes promptly when the last child
// AWSServiceAccountRole disappears, instead of waiting on the reconcileDelete
// safety-net requeue. Create/Update/Generic cannot relax the "roles remain"
// gate, so dropping them avoids gratuitous parent reconciles.
func roleDeletedForConfigPredicate(controllerName string) predicate.Predicate {
	return metricsRecordingPredicate(controllerName, predicateKeepFns{
		Delete: func(_ event.DeleteEvent) bool { return true },
	})
}

// ackChildCRDChangedPredicate fires only when crdName transitions to
// Established=True, so a late-installed optional ACK CRD triggers the
// initial parent reconcile without flooding the queue.
func ackChildCRDChangedPredicate(controllerName, crdName string) predicate.Predicate {
	return metricsRecordingPredicate(controllerName, predicateKeepFns{
		Create: func(e event.CreateEvent) bool {
			return isAckChildCRDEstablished(e.Object, crdName)
		},
		Update: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}

			return !isAckChildCRDEstablished(e.ObjectOld, crdName) && isAckChildCRDEstablished(e.ObjectNew, crdName)
		},
		Delete:  nil,
		Generic: nil,
	})
}

// siblingServiceAccountBindingChangedPredicate selects the AWSServiceAccountRole
// events that can move another role in or out of a duplicate-binding conflict:
//   - Create: a new role may form a new conflict.
//   - Delete: a deleted role may resolve an existing conflict.
//   - Update: only deletion-timestamp transitions matter. spec.serviceAccount
//     is immutable on AWSServiceAccountRole (CEL marker on the spec), and
//     reconcileDuplicateBindingConflict ignores siblings whose
//     DeletionTimestamp is non-zero, so entering termination is the trigger
//     for surviving siblings to clear status.
func siblingServiceAccountBindingChangedPredicate(controllerName string) predicate.Predicate {
	return metricsRecordingPredicate(controllerName, predicateKeepFns{
		Create: func(_ event.CreateEvent) bool { return true },
		Update: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}

			return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
		Delete: func(_ event.DeleteEvent) bool { return true },
	})
}

func isAckChildCRDEstablished(obj client.Object, crdName string) bool {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok || crd.Name != crdName {
		return false
	}

	for _, c := range crd.Status.Conditions {
		if c.Type == apiextensionsv1.Established {
			return c.Status == apiextensionsv1.ConditionTrue
		}
	}

	return false
}
