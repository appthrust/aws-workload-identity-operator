// Package v1alpha1_test contains regression coverage for the CRD schema
// applied to the AWSServiceAccountRole spec.policyDocument string field and
// for the spec-level XValidation rule that requires exactly one of
// spec.policyARNs / spec.policyDocument.
//
// The tests evaluate the generated CRD schema directly (no envtest) by:
//
//  1. Parsing config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml.
//  2. Extracting the spec sub-schema.
//  3. Converting to a structural schema and running the OpenAPI validator
//     (for keywords like minLength / maxLength) plus the CEL validator
//     (for x-kubernetes-validations) against representative inputs.
//
// The CRD YAML is the contract enforced by kube-apiserver, so exercising
// the rendered schema gives the same coverage as a real Create() round-trip
// without spinning up etcd/envtest.
package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

const roleCRDPath = "../../config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml"

// loadRoleSpecSchema reads the rendered AWSServiceAccountRole CRD and returns
// the spec sub-schema converted to the internal apiextensions representation.
// The internal representation is required by both the structural-schema
// constructor and the OpenAPI SchemaValidator.
func loadRoleSpecSchema(t *testing.T) *apiextensionsinternal.JSONSchemaProps {
	t.Helper()

	abs, err := filepath.Abs(roleCRDPath)
	if err != nil {
		t.Fatalf("resolve CRD path: %v", err)
	}
	// G304: roleCRDPath is a compile-time const pointing at the generated
	// CRD inside this repo; this test cannot be reached with an
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

	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&specSchema, internal, nil); err != nil {
		t.Fatalf("convert spec schema to internal: %v", err)
	}

	return internal
}

// validateRoleSpec runs the OpenAPI + CEL validation pipeline that
// kube-apiserver applies to the AWSServiceAccountRole spec on Create/Update.
func validateRoleSpec(t *testing.T, obj map[string]interface{}) field.ErrorList {
	t.Helper()

	internalSchema := loadRoleSpecSchema(t)

	openAPIValidator, _, err := apiservervalidation.NewSchemaValidator(internalSchema)
	if err != nil {
		t.Fatalf("build openapi validator: %v", err)
	}

	errs := apiservervalidation.ValidateCustomResource(field.NewPath("spec"), obj, openAPIValidator)

	structural, err := schema.NewStructural(internalSchema)
	if err != nil {
		t.Fatalf("build structural schema: %v", err)
	}

	celValidator := celschema.NewValidator(structural, false, celconfig.PerCallLimit)
	if celValidator != nil {
		celErrs, _ := celValidator.Validate(context.Background(), field.NewPath("spec"), structural, obj, nil, celconfig.RuntimeCELCostBudget)
		errs = append(errs, celErrs...)
	}

	return errs
}

// validServiceAccount returns a minimal-valid spec.serviceAccount block so
// tests can focus on policyDocument behaviour without tripping the
// serviceAccount required/pattern checks.
func validServiceAccount() map[string]interface{} {
	return map[string]interface{}{
		"namespace": "default",
		"name":      "demo",
	}
}

// minimalPolicyDocument returns a well-formed IAM policy JSON string short
// enough to clear the MaxLength bound.
func minimalPolicyDocument() string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"arn:aws:s3:::example-bucket/*"}]}`
}

func TestRoleSpecAcceptsValidIAMPolicyDocument(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": minimalPolicyDocument(),
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) != 0 {
		t.Fatalf("expected valid IAM policy document string to be accepted, got %v", errs)
	}
}

func TestRoleSpecRejectsEmptyPolicyDocument(t *testing.T) {
	// MinLength=1 rejects an empty string before the XOR XValidation evaluates.
	// Without this floor, has(self.policyDocument) returns true for "" and the
	// XOR rule fires with a confusing message; the explicit MinLength keeps
	// the failure focused on the empty-string case.
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": "",
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected empty policyDocument string to be rejected, got no errors")
	}

	if !containsErr(errs, "should be at least 1") && !containsErr(errs, "min length") && !containsErr(errs, "policyARNs") {
		t.Fatalf("expected MinLength or XOR rejection, got %v", errs)
	}
}

func TestRoleSpecRejectsOversizedPolicyDocument(t *testing.T) {
	// AWS customer-managed IAM policy hard limit is 6144 characters. MaxLength
	// mirrors that limit so admission rejects payloads that AWS would also
	// reject, and bounds the stored object size per api-conventions.md.
	oversized := strings.Repeat("a", 6145)

	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": oversized,
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected 6145-char policyDocument to be rejected by MaxLength=6144, got no errors")
	}

	if !containsErr(errs, "6144") {
		t.Fatalf("expected error mentioning the 6144-char cap, got %v", errs)
	}
}

func TestRoleSpecAcceptsPolicyDocumentAtMaxLength(t *testing.T) {
	// A payload exactly at the 6144-char ceiling must still be accepted; this
	// guards against off-by-one regressions on the MaxLength marker.
	prefix := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"`
	suffix := `"}]}`

	pad := 6144 - len(prefix) - len(suffix)
	if pad <= 0 {
		t.Fatalf("test fixture overflowed before padding; prefix+suffix already exceeds 6144 chars")
	}

	atLimit := prefix + strings.Repeat("a", pad) + suffix
	if len(atLimit) != 6144 {
		t.Fatalf("expected fixture length 6144, got %d", len(atLimit))
	}

	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": atLimit,
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) != 0 {
		t.Fatalf("expected policyDocument at MaxLength=6144 to be accepted, got %v", errs)
	}
}

func TestRoleSpecRejectsMissingPolicyARNsAndDocument(t *testing.T) {
	// Sanity guard: the spec-level XValidation rule
	// `(has(self.policyARNs) && self.policyARNs.size() > 0) != has(self.policyDocument)`
	// must still reject specs that supply neither field.
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected spec without policyARNs/policyDocument to be rejected, got no errors")
	}

	if !containsErr(errs, "policyARNs") || !containsErr(errs, "policyDocument") {
		t.Fatalf("expected XValidation message referencing policyARNs/policyDocument, got %v", errs)
	}
}

func TestRoleSpecRejectsBothPolicyARNsAndDocument(t *testing.T) {
	// Guards the spec-level CEL rule against regressing back to OR semantics,
	// which would let a spec set both policyARNs and policyDocument
	// simultaneously and produce silently divergent IAM delivery.
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyARNs": []interface{}{
			"arn:aws:iam::123456789012:policy/example",
		},
		"policyDocument": minimalPolicyDocument(),
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected spec with both policyARNs and policyDocument to be rejected by the XOR rule, got no errors")
	}

	if !containsErr(errs, "policyARNs") || !containsErr(errs, "policyDocument") {
		t.Fatalf("expected XValidation message referencing policyARNs/policyDocument, got %v", errs)
	}
}
