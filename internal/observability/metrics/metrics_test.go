package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordWatchMapListErrorIncrementsCounter(t *testing.T) {
	counter := watchMapListErrorsTotal.WithLabelValues(
		ControllerRoleReplicaSet,
		"replicaSetsForNamespace",
		"Namespace",
	)

	before := testutil.ToFloat64(counter)

	RecordWatchMapListError(ControllerRoleReplicaSet, "replicaSetsForNamespace", "Namespace")

	after := testutil.ToFloat64(counter)
	if after-before != 1 {
		t.Fatalf("expected watchMapListErrorsTotal to increment by 1, got delta %v (before=%v after=%v)", after-before, before, after)
	}
}

func TestRecordWatchMapListErrorAppliesStableLabel(t *testing.T) {
	// A label value containing characters outside the stableLabel allowlist
	// must be coerced to labelOther so unbounded user-controlled strings can
	// never leak into Prometheus cardinality.
	RecordWatchMapListError("controller with spaces", "replicaSetsForNamespace", "Namespace")

	counter := watchMapListErrorsTotal.WithLabelValues(
		labelOther,
		"replicaSetsForNamespace",
		"Namespace",
	)

	if got := testutil.ToFloat64(counter); got < 1 {
		t.Fatalf("expected watchMapListErrorsTotal{controller=%q,...} >= 1, got %v", labelOther, got)
	}
}
