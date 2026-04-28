package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

const (
	resultError   = "error"
	resultSuccess = "success"
	labelOther    = "other"
	labelUnknown  = "unknown"
)

var (
	childApplyTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "awio_child_apply_total",
			Help: "Total child resource apply operations by controller, child kind, operation, and result.",
		},
		[]string{"controller", "child_kind", "operation", "result"},
	)
	conditionTransitionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "awio_condition_transition_total",
			Help: "Total status condition transitions by resource kind, condition, status, and reason.",
		},
		[]string{"kind", "condition", "status", "reason"},
	)
	remoteDeliveryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "awio_remote_delivery_total",
			Help: "Total remote delivery operations by delivery type, resource, result, and stable reason.",
		},
		[]string{"delivery_type", "resource", "result", "reason"},
	)
	predicateDecisionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "awio_predicate_decision_total",
			Help: "Total predicate decisions by controller and bounded decision.",
		},
		[]string{"controller", "decision"},
	)
	watchMapListErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "awio_watch_map_list_errors_total",
			Help: "Total LIST errors from controller watch map functions by controller, map function, and kind.",
		},
		[]string{"controller", "map_func", "kind"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		childApplyTotal,
		conditionTransitionTotal,
		remoteDeliveryTotal,
		predicateDecisionTotal,
		watchMapListErrorsTotal,
	)
}

// RecordChildApply records one create-or-update operation for a bounded child kind.
func RecordChildApply(controller, childKind string, operation controllerutil.OperationResult, err error) {
	result := resultSuccess
	if err != nil {
		result = resultError
	}

	childApplyTotal.WithLabelValues(controller, childKind, stableOperation(operation), result).Inc()
}

// RecordConditionTransition records status/reason transitions only; condition messages
// can include user input or provider errors and are intentionally excluded.
func RecordConditionTransition(kind string, condition *metav1.Condition) {
	conditionTransitionTotal.WithLabelValues(
		kind,
		stableLabel(condition.Type),
		stableLabel(string(condition.Status)),
		stableReason(condition.Reason),
	).Inc()
}

// RecordRemoteDelivery records one remote self-hosted delivery action.
func RecordRemoteDelivery(deliveryType, resource, result, reason string) {
	remoteDeliveryTotal.WithLabelValues(
		stableDeliveryType(deliveryType),
		stableLabel(resource),
		stableResult(result),
		stableReason(reason),
	).Inc()
}

// RecordPredicateDecision records one bounded predicate keep/drop decision.
func RecordPredicateDecision(controller, decision string) {
	predicateDecisionTotal.WithLabelValues(
		stableLabel(controller),
		stablePredicateDecision(decision),
	).Inc()
}

// RecordWatchMapListError records one LIST failure inside a controller-runtime
// EnqueueRequestsFromMapFunc callback. MapFunc cannot return errors, so a
// failed LIST silently drops the source event; the counter lets on-call alert
// on the failure rate.
func RecordWatchMapListError(controller, mapFunc, kind string) {
	watchMapListErrorsTotal.WithLabelValues(
		stableLabel(controller),
		stableLabel(mapFunc),
		stableLabel(kind),
	).Inc()
}

func stableOperation(operation controllerutil.OperationResult) string {
	value := string(operation)
	if value == "" {
		return labelUnknown
	}

	return stableLabel(value)
}

var (
	deliveryTypeAllowlist = sets.New[string](allDeliveryTypes...)
	resultAllowlist       = sets.New[string](resultSuccess, resultError, RemoteDeliveryResultSkipped)
	decisionAllowlist     = sets.New[string](PredicateDecisionKept, PredicateDecisionDropped)
	reasonAllowlist       = buildReasonAllowlist()
)

func buildReasonAllowlist() sets.Set[string] {
	allowlist := sets.New[string]()
	allowlist.Insert(identityv1.AllReasons()...)
	allowlist.Insert(AllRemoteDeliveryReasons...)
	allowlist.Insert(
		string(controllerutil.OperationResultCreated),
		string(controllerutil.OperationResultUpdated),
		string(controllerutil.OperationResultUpdatedStatus),
		string(controllerutil.OperationResultUpdatedStatusOnly),
		string(controllerutil.OperationResultNone),
	)

	return allowlist
}

func allowlisted(value string, allowed sets.Set[string]) string {
	if value == "" {
		return labelUnknown
	}

	if allowed.Has(value) {
		return value
	}

	return labelOther
}

func stableDeliveryType(deliveryType string) string {
	return allowlisted(deliveryType, deliveryTypeAllowlist)
}

func stableResult(result string) string {
	return allowlisted(result, resultAllowlist)
}

func stablePredicateDecision(decision string) string {
	return allowlisted(decision, decisionAllowlist)
}

func stableReason(reason string) string {
	return allowlisted(reason, reasonAllowlist)
}

func stableLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return labelUnknown
	}

	if len(value) > 64 {
		return labelOther
	}

	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}

		return labelOther
	}

	return value
}
