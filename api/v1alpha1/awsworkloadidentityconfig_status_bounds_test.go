// Package v1alpha1_test pins the OpenAPI keyword validations attached to
// status identifier strings on the AWSWorkloadIdentityConfig CRD.
//
// Status payloads are controller-written, but the CRD schema is the same
// contract the apiserver enforces on every write, so bounding status strings
// rejects pathological values (oversized payloads, drift between operator
// and AWS shape) before they reach etcd. This file is a shape-only guard:
// if a marker is removed or loosened in a future refactor, the test fires
// before any semantic regression slips into a release.
package v1alpha1_test

import (
	"testing"
)

// Expected literal Pattern strings the kubebuilder markers on
// AWSWorkloadIdentityConfigStatus identifier fields must produce.
// Intentional marker edits MUST update these literals in the same commit.
const (
	expectedConfigStatusIssuerHostPathPattern  = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(/[A-Za-z0-9._~%!$&'()*+,;=:@/-]*)?$`
	expectedConfigStatusOIDCProviderARNPattern = `^arn:aws[a-z0-9-]*:iam::[0-9]{12}:oidc-provider/[A-Za-z0-9._~%!$&'()*+,;=:@/-]+$`
	// ResolvedClusterName is `<namespace>/<name>` from
	// `types.NamespacedName.String()`, so the pattern admits two DNS-1123
	// subdomain segments separated by a slash.
	expectedConfigStatusResolvedClusterNamePattern     = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	expectedConfigStatusWebhookRuntimeNamespacePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
)

func TestWorkloadIdentityConfigStatusIdentifierBounds(t *testing.T) {
	status := loadCRDStatusSchema(t, workloadIdentityConfigCRDPath)

	cases := []struct {
		field           string
		expectedPattern string
		expectedMaxLen  int64
	}{
		{"issuerHostPath", expectedConfigStatusIssuerHostPathPattern, 2048},
		{"oidcProviderARN", expectedConfigStatusOIDCProviderARNPattern, 2048},
		{"resolvedClusterName", expectedConfigStatusResolvedClusterNamePattern, 507},
		{"webhookRuntimeNamespace", expectedConfigStatusWebhookRuntimeNamespacePattern, 63},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			schema, ok := status.Properties[tc.field]
			if !ok {
				t.Fatalf("status.%s property missing from generated CRD", tc.field)
			}

			if schema.Pattern != tc.expectedPattern {
				t.Fatalf("status.%s pattern drift:\n  want: %s\n  got:  %s", tc.field, tc.expectedPattern, schema.Pattern)
			}

			if schema.MaxLength == nil || *schema.MaxLength != tc.expectedMaxLen {
				t.Fatalf("status.%s maxLength must be %d, got %+v", tc.field, tc.expectedMaxLen, schema.MaxLength)
			}
		})
	}
}
