// Package v1alpha1_test pins the OpenAPI keyword validations attached to
// spec.eksIRSA.oidcProvider.arn on the AWSWorkloadIdentityConfig CRD.
//
// The ARN field is bounded by MinLength=1, MaxLength=2048, and a Pattern that
// matches canonical IAM OIDC provider ARNs across all AWS partitions. The
// MaxLength bound (added alongside this test) pins the field length against
// the practical IAM ARN ceiling so that the apiserver rejects pathological
// inputs before they reach AWS API calls.
//
// This file is intentionally a shape-only test: accept/reject behaviour
// against representative ARNs is exercised by the controller integration and
// e2e layers, which already round-trip live CRD validation. Here we guard
// only the literal marker values rendered into the generated CRD, so that
// marker drift fires before any semantic regression.
package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// expectedOIDCProviderARNPattern is the exact Pattern literal the kubebuilder
// marker on EKSIRSAOIDCProviderConfig.ARN must produce in the generated CRD.
// If you intentionally change the marker, update this literal in the same
// commit.
const expectedOIDCProviderARNPattern = `^arn:aws[a-z0-9-]*:iam::[0-9]{12}:oidc-provider/[A-Za-z0-9._~%!$&'()*+,;=:@/-]+$`

// loadOIDCProviderARNSchema reads the rendered CRD and returns the
// spec.eksIRSA.oidcProvider.arn leaf sub-schema. The shape test only needs
// the v1 representation; no apiserver validator pipeline is wired here.
func loadOIDCProviderARNSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	abs, err := filepath.Abs(workloadIdentityConfigCRDPath)
	if err != nil {
		t.Fatalf("resolve CRD path: %v", err)
	}
	// G304: workloadIdentityConfigCRDPath is a compile-time const pointing at
	// the generated CRD inside this repo; this test cannot be reached with an
	// attacker-controlled path.
	raw, err := os.ReadFile(abs) //nolint:gosec // const path inside repo tree
	if err != nil {
		t.Fatalf("read CRD %s: %v", abs, err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096).Decode(&crd); err != nil {
		t.Fatalf("decode CRD: %v", err)
	}

	if len(crd.Spec.Versions) == 0 || crd.Spec.Versions[0].Schema == nil || crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
		t.Fatalf("CRD has no openAPIV3Schema; controller-gen output may have regressed")
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	specSchema, ok := root.Properties["spec"]
	if !ok {
		t.Fatalf("CRD root has no spec property")
	}

	eksIRSASchema, ok := specSchema.Properties["eksIRSA"]
	if !ok {
		t.Fatalf("CRD spec has no eksIRSA property")
	}

	oidcProviderSchema, ok := eksIRSASchema.Properties["oidcProvider"]
	if !ok {
		t.Fatalf("CRD spec.eksIRSA has no oidcProvider property")
	}

	arnSchema, ok := oidcProviderSchema.Properties["arn"]
	if !ok {
		t.Fatalf("CRD spec.eksIRSA.oidcProvider has no arn property")
	}

	return arnSchema
}

// TestOIDCProviderARNSchemaShape is a literal shape test against the rendered
// CRD: if the kubebuilder markers on EKSIRSAOIDCProviderConfig.ARN are
// loosened or otherwise altered, this test fires before any semantic
// behaviour changes.
func TestOIDCProviderARNSchemaShape(t *testing.T) {
	arnSchema := loadOIDCProviderARNSchema(t)

	if arnSchema.Pattern != expectedOIDCProviderARNPattern {
		t.Fatalf("spec.eksIRSA.oidcProvider.arn pattern drift:\n  want: %s\n  got:  %s", expectedOIDCProviderARNPattern, arnSchema.Pattern)
	}

	if arnSchema.MinLength == nil || *arnSchema.MinLength != 1 {
		t.Fatalf("spec.eksIRSA.oidcProvider.arn minLength must be 1, got %+v", arnSchema.MinLength)
	}

	if arnSchema.MaxLength == nil || *arnSchema.MaxLength != 2048 {
		t.Fatalf("spec.eksIRSA.oidcProvider.arn maxLength must be 2048, got %+v", arnSchema.MaxLength)
	}
}
