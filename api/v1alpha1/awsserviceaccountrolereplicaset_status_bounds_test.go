// Package v1alpha1_test pins the OpenAPI keyword validations attached to
// status identifier strings on the AWSServiceAccountRoleReplicaSet CRD.
//
// Status payloads are controller-written, but the CRD schema is the same
// contract the apiserver enforces on every write, so bounding status strings
// rejects pathological values (oversized payloads, drift between operator
// and OCM shape) before they reach etcd. This file is a shape-only guard:
// if a marker is removed or loosened in a future refactor, the test fires
// before any semantic regression slips into a release.
package v1alpha1_test

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Expected literal Pattern strings the kubebuilder markers on
// AWSServiceAccountRoleReplicaSetStatus identifier fields must produce.
// Intentional marker edits MUST update these literals in the same commit.
const expectedReplicaSetStatusDNS1123SubdomainPattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`

// requireItemSchema descends into an array property and returns the per-item
// schema. The replicaSet status list types use listType=map so each item
// schema carries the per-element kubebuilder markers.
func requireItemSchema(t *testing.T, parent *apiextensionsv1.JSONSchemaProps, field string) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	arr, ok := parent.Properties[field]
	if !ok {
		t.Fatalf("status.%s property missing from generated CRD", field)
	}

	if arr.Items == nil || arr.Items.Schema == nil {
		t.Fatalf("status.%s has no item schema", field)
	}

	return *arr.Items.Schema
}

func TestReplicaSetStatusIdentifierBounds(t *testing.T) {
	status := loadCRDStatusSchema(t, replicaSetCRDPath)

	placementItem := requireItemSchema(t, &status, "placements")
	failedItem := requireItemSchema(t, &status, "failedClusters")
	clusterItem := requireItemSchema(t, &status, "clusters")

	type fieldCase struct {
		parentPath      string
		parent          apiextensionsv1.JSONSchemaProps
		field           string
		expectedPattern string
		expectedMaxLen  int64
	}

	cases := []fieldCase{
		{"status.placements[]", placementItem, "name", expectedReplicaSetStatusDNS1123SubdomainPattern, 253},
		// availableDecisionGroups is intentionally pattern-free (free-form OCM
		// summary); only MaxLength is asserted.
		{"status.failedClusters[]", failedItem, "clusterName", expectedReplicaSetStatusDNS1123SubdomainPattern, 253},
		{"status.clusters[]", clusterItem, "clusterName", expectedReplicaSetStatusDNS1123SubdomainPattern, 253},
		// ClusterSummary.Namespace is written verbatim from the OCM
		// ManagedCluster name, so the bound mirrors clusterName, not the
		// stricter DNS-1123 label that a downstream Kubernetes namespace
		// would carry.
		{"status.clusters[]", clusterItem, "namespace", expectedReplicaSetStatusDNS1123SubdomainPattern, 253},
		{"status.clusters[]", clusterItem, "name", expectedReplicaSetStatusDNS1123SubdomainPattern, 253},
	}

	for _, tc := range cases {
		t.Run(tc.parentPath+"."+tc.field, func(t *testing.T) {
			schema, ok := tc.parent.Properties[tc.field]
			if !ok {
				t.Fatalf("%s.%s property missing from generated CRD", tc.parentPath, tc.field)
			}

			if schema.Pattern != tc.expectedPattern {
				t.Fatalf("%s.%s pattern drift:\n  want: %s\n  got:  %s", tc.parentPath, tc.field, tc.expectedPattern, schema.Pattern)
			}

			if schema.MaxLength == nil || *schema.MaxLength != tc.expectedMaxLen {
				t.Fatalf("%s.%s maxLength must be %d, got %+v", tc.parentPath, tc.field, tc.expectedMaxLen, schema.MaxLength)
			}
		})
	}

	t.Run("status.placements[].availableDecisionGroups maxLength", func(t *testing.T) {
		schema, ok := placementItem.Properties["availableDecisionGroups"]
		if !ok {
			t.Fatalf("status.placements[].availableDecisionGroups property missing from generated CRD")
		}

		if schema.MaxLength == nil || *schema.MaxLength != 1024 {
			t.Fatalf("status.placements[].availableDecisionGroups maxLength must be 1024, got %+v", schema.MaxLength)
		}
	})
}

// rolloutCounterFields names the int counters every
// AWSServiceAccountRoleRolloutSummary instance carries.
var rolloutCounterFields = []string{"total", "updating", "succeeded", "failed", "timedOut"}

// TestReplicaSetStatusCounterMinimums pins minimum=0 on every int counter the
// controller writes into AWSServiceAccountRoleReplicaSetStatus. Counters are
// derived from set cardinalities and ++ increments, so a negative value would
// indicate either a controller bug or an etcd-side write outside the operator;
// the apiserver-enforced minimum surfaces both before they reach status
// consumers. api-conventions.md requires numeric fields to be bounds-checked
// (sig-architecture/api-conventions.md: "All numeric fields should be
// bounds-checked, both for too-small values (including negative) and for
// too-large values.").
func TestReplicaSetStatusCounterMinimums(t *testing.T) {
	status := loadCRDStatusSchema(t, replicaSetCRDPath)

	assertReplicaSetTopLevelCounterMinimums(t, &status)
	assertReplicaSetRolloutCounterMinimums(t, &status)
	assertReplicaSetPlacementCounterMinimums(t, &status)
}

func assertReplicaSetTopLevelCounterMinimums(t *testing.T, status *apiextensionsv1.JSONSchemaProps) {
	t.Helper()

	fields := []string{
		"selectedClusterCount",
		"desiredClusterCount",
		"appliedClusterCount",
		"readyClusterCount",
		"staleClusterCount",
		"conflictCount",
		"failureCount",
		"observedGeneration",
	}

	for _, field := range fields {
		t.Run("status."+field, func(t *testing.T) {
			requirePropertyMinimumZero(t, "status."+field, status, field)
		})
	}
}

func assertReplicaSetRolloutCounterMinimums(t *testing.T, status *apiextensionsv1.JSONSchemaProps) {
	t.Helper()

	t.Run("status.rollout", func(t *testing.T) {
		rollout, ok := status.Properties["rollout"]
		if !ok {
			t.Fatalf("status.rollout property missing from generated CRD")
		}

		for _, field := range rolloutCounterFields {
			requirePropertyMinimumZero(t, "status.rollout."+field, &rollout, field)
		}
	})
}

func assertReplicaSetPlacementCounterMinimums(t *testing.T, status *apiextensionsv1.JSONSchemaProps) {
	t.Helper()

	placementItem := requireItemSchema(t, status, "placements")

	t.Run("status.placements[].selectedClusterCount", func(t *testing.T) {
		requirePropertyMinimumZero(t, "status.placements[].selectedClusterCount", &placementItem, "selectedClusterCount")
	})

	t.Run("status.placements[].rollout", func(t *testing.T) {
		rollout, ok := placementItem.Properties["rollout"]
		if !ok {
			t.Fatalf("status.placements[].rollout property missing from generated CRD")
		}

		for _, field := range rolloutCounterFields {
			requirePropertyMinimumZero(t, "status.placements[].rollout."+field, &rollout, field)
		}
	})
}

func requirePropertyMinimumZero(t *testing.T, path string, parent *apiextensionsv1.JSONSchemaProps, field string) {
	t.Helper()

	schema, ok := parent.Properties[field]
	if !ok {
		t.Fatalf("%s property missing from generated CRD", path)
	}

	if schema.Minimum == nil {
		t.Fatalf("%s must declare minimum=0 (api-conventions: non-negative integer); got nil", path)
	}

	if *schema.Minimum != 0 {
		t.Fatalf("%s minimum must be 0, got %v", path, *schema.Minimum)
	}
}
