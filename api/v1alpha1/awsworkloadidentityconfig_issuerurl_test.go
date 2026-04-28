// Package v1alpha1_test contains regression coverage for the OpenAPI
// keyword validations attached to spec.eksIRSA.issuerURL on the
// AWSWorkloadIdentityConfig CRD.
//
// History: the original Pattern marker accepted any HTTPS URL, which let
// non-EKS issuers (e.g. self-hosted S3-backed issuers) slip through Pattern
// validation. The marker was tightened to admit only the canonical EKS
// OIDC issuer URL shape:
//
//	https://oidc.eks.<region>.<partition-host>/id/<UPPER-ALPHANUM>
//
// The Pattern intentionally does not enumerate AWS partition TLDs. spec.region
// already accepts commercial, GovCloud, China, ISO/ISOB/ISOE, and EUSC region
// strings; a TLD allowlist on issuerURL would create a region/issuer mismatch
// where the same CR is admitted by region and rejected by issuerURL. Instead,
// typo defence is concentrated in the `oidc.eks.` prefix and `/id/<UPPER-ALPHANUM>`
// path structure, while the host TLD part `[a-z0-9.-]+` absorbs all current
// and future AWS partition hosts.
//
// These tests pin both:
//
//  1. The exact Pattern literal rendered into the generated CRD YAML
//     (shape test) - if controller-gen output drifts or the marker is
//     loosened, this guard fires.
//  2. The semantic acceptance/rejection behaviour of the rendered Pattern
//     against representative inputs, by running the apiserver's OpenAPI
//     validator against the leaf schema.
//
// The CRD YAML is the contract the apiserver enforces, so testing against
// the rendered schema gives the same coverage as a real kube-apiserver
// round-trip without spinning up envtest.
package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// expectedIssuerURLPattern is the exact Pattern literal the kubebuilder
// marker on EKSIRSAConfig.IssuerURL must produce in the generated CRD.
// If you intentionally change the marker, update both this literal and the
// semantic cases below in the same commit.
const expectedIssuerURLPattern = `^https://oidc\.eks\.[a-z]{2,}-[a-z0-9-]+-[0-9]+\.[a-z0-9.-]+/id/[A-Z0-9]+$`

// loadIssuerURLSchema reads the rendered CRD and returns the
// spec.eksIRSA.issuerURL leaf sub-schema converted to the internal
// apiextensions representation, which is required by the OpenAPI
// SchemaValidator.
func loadIssuerURLSchema(t *testing.T) (apiextensionsv1.JSONSchemaProps, *apiextensionsinternal.JSONSchemaProps) {
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

	v1Schema := navigateToIssuerURL(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)

	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&v1Schema, internal, nil); err != nil {
		t.Fatalf("convert issuerURL schema to internal: %v", err)
	}

	return v1Schema, internal
}

func navigateToIssuerURL(t *testing.T, root *apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	specSchema, ok := root.Properties["spec"]
	if !ok {
		t.Fatalf("CRD root has no spec property")
	}

	eksIRSASchema, ok := specSchema.Properties["eksIRSA"]
	if !ok {
		t.Fatalf("CRD spec has no eksIRSA property")
	}

	issuerURLSchema, ok := eksIRSASchema.Properties["issuerURL"]
	if !ok {
		t.Fatalf("CRD spec.eksIRSA has no issuerURL property")
	}

	return issuerURLSchema
}

// validateIssuerURL runs the OpenAPI validation pipeline that the
// kube-apiserver applies to spec.eksIRSA.issuerURL during Create/Update.
func validateIssuerURL(t *testing.T, value string) field.ErrorList {
	t.Helper()

	_, internalSchema := loadIssuerURLSchema(t)

	openAPIValidator, _, err := apiservervalidation.NewSchemaValidator(internalSchema)
	if err != nil {
		t.Fatalf("build openapi validator: %v", err)
	}

	return apiservervalidation.ValidateCustomResource(field.NewPath("spec", "eksIRSA", "issuerURL"), value, openAPIValidator)
}

// TestIssuerURLPatternShape is a literal shape test against the rendered
// CRD: if the kubebuilder Pattern marker on EKSIRSAConfig.IssuerURL is
// loosened or otherwise altered, this test fires before any semantic
// behaviour changes.
func TestIssuerURLPatternShape(t *testing.T) {
	v1Schema, _ := loadIssuerURLSchema(t)

	if v1Schema.Pattern != expectedIssuerURLPattern {
		t.Fatalf("spec.eksIRSA.issuerURL pattern drift:\n  want: %s\n  got:  %s", expectedIssuerURLPattern, v1Schema.Pattern)
	}

	if v1Schema.MinLength == nil || *v1Schema.MinLength != 1 {
		t.Fatalf("spec.eksIRSA.issuerURL minLength must be 1, got %+v", v1Schema.MinLength)
	}

	if v1Schema.MaxLength == nil || *v1Schema.MaxLength != 256 {
		t.Fatalf("spec.eksIRSA.issuerURL maxLength must be 256, got %+v", v1Schema.MaxLength)
	}
}

func TestIssuerURLAcceptsCanonicalEKSIssuers(t *testing.T) {
	cases := []string{
		"https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		"https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE",
		"https://oidc.eks.us-gov-west-1.amazonaws.com/id/ABC123",
		"https://oidc.eks.cn-north-1.amazonaws.com.cn/id/CHINA1",
		// ISO/ISOB partition issuers. spec.region accepts these regions, so the
		// Pattern on issuerURL must also accept their partition TLDs to avoid
		// a region/issuer admission mismatch.
		"https://oidc.eks.us-iso-east-1.c2s.ic.gov/id/ISO1",
		"https://oidc.eks.us-isob-east-1.sc2s.sgov.gov/id/ISOB1",
		// MaxLength=256 upper-boundary: exactly 256 chars must be accepted.
		// Canonical prefix up to and including "/id/" is 49 chars, so 207
		// 'A' chars in the id segment make the URL exactly 256 chars.
		"https://oidc.eks.ap-northeast-1.amazonaws.com/id/" + strings.Repeat("A", 207),
	}

	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			errs := validateIssuerURL(t, url)
			if len(errs) != 0 {
				t.Fatalf("expected issuerURL %q to be accepted, got %v", url, errs)
			}
		})
	}
}

func TestIssuerURLRejectsNonEKSIssuers(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{
			name:  "http scheme rejected",
			value: "http://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		},
		{
			name:  "generic https host rejected",
			value: "https://issuer.example.com/foo",
		},
		{
			// The `oidc.eks.` literal prefix is the typo-catch anchor, so
			// hosts missing that prefix must still be rejected even though
			// the host TLD portion is intentionally permissive.
			name:  "missing oidc.eks. prefix rejected",
			value: "https://eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		},
		{
			name:  "lowercase id segment rejected",
			value: "https://oidc.eks.ap-northeast-1.amazonaws.com/id/example",
		},
		{
			name:  "trailing slash rejected",
			value: "https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE/",
		},
		{
			name:  "missing /id/ segment rejected",
			value: "https://oidc.eks.ap-northeast-1.amazonaws.com/EXAMPLE",
		},
		{
			// Confirms the new Pattern disallows :443. If product intent
			// shifts to admit explicit :443, both the kubebuilder marker
			// and this case must change in the same commit.
			name:  "explicit port :443 rejected",
			value: "https://oidc.eks.ap-northeast-1.amazonaws.com:443/id/EXAMPLE",
		},
		{
			name:  "empty rejected by MinLength",
			value: "",
		},
		{
			// MaxLength=256 boundary: 257 chars must be rejected. The
			// canonical prefix up to and including "/id/" is 49 chars
			// ("https://oidc.eks.ap-northeast-1.amazonaws.com/id/"), so we
			// pad the id segment with 208 'A' characters to reach 257.
			name:  "exceeds MaxLength rejected",
			value: "https://oidc.eks.ap-northeast-1.amazonaws.com/id/" + strings.Repeat("A", 208),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIssuerURL(t, tc.value)
			if len(errs) == 0 {
				t.Fatalf("expected issuerURL %q to be rejected, got no errors", tc.value)
			}
		})
	}
}
