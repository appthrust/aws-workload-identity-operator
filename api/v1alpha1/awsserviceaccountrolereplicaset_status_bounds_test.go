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
