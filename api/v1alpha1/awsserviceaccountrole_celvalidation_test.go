// Package v1alpha1_test contains regression coverage for the bounded
// structural schema that the AWSServiceAccountRole CRD applies to its
// spec.policyDocument field, plus the existing top-level XValidation rule
// that requires at least one of spec.policyARNs / spec.policyDocument.
//
// The tests evaluate the generated CRD schema directly (no envtest) by:
//
//  1. Parsing config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml.
//  2. Extracting either the spec sub-schema or the spec.policyDocument
//     sub-schema as needed.
//  3. Converting to a structural schema and running the OpenAPI validator
//     (for keywords like enum / maxLength / minItems / maxItems /
//     maxProperties) plus the CEL validator (for x-kubernetes-validations)
//     against representative inputs.
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
// It returns the combined field errors so tests can inspect both the
// OpenAPI structural keywords (enum / maxLength / minItems / maxItems /
// maxProperties) and the CEL XValidation rules at the spec level.
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

// minimalStatement returns one IAM-shaped statement object. It uses canonical
// IAM polymorphic keys (Effect/Action/Resource) so we exercise the
// XPreserveUnknownFields path on each Statement item.
func minimalStatement() map[string]interface{} {
	return map[string]interface{}{
		"Effect":   "Allow",
		"Action":   []interface{}{"s3:GetObject"},
		"Resource": "arn:aws:s3:::example-bucket/*",
	}
}

func TestRoleSpecAcceptsValidIAMPolicyDocument(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{
			"Version": "2012-10-17",
			"Id":      "demo-policy",
			"Statement": []interface{}{
				minimalStatement(),
			},
		},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) != 0 {
		t.Fatalf("expected valid IAM policy document to be accepted, got %v", errs)
	}
}

func TestRoleSpecRejectsUnknownPolicyVersion(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{
			"Version": "2099-01-01",
			"Statement": []interface{}{
				minimalStatement(),
			},
		},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected policyDocument.Version outside enum to be rejected, got no errors")
	}
	// kube-openapi surfaces enum violations with `supported values:` or
	// `Unsupported value` depending on the validator path; either is
	// acceptable evidence that the enum is enforced.
	if !containsErr(errs, "2012-10-17") && !containsErr(errs, "supported values") && !containsErr(errs, "Unsupported value") {
		t.Fatalf("expected error mentioning the Version enum, got %v", errs)
	}
}

func TestRoleSpecRejectsEmptyStatementList(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{
			"Version":   "2012-10-17",
			"Statement": []interface{}{},
		},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected empty Statement list to be rejected by MinItems=1, got no errors")
	}

	if !containsErr(errs, "minimum") && !containsErr(errs, "should have at least 1") {
		t.Fatalf("expected error mentioning MinItems, got %v", errs)
	}
}

func TestRoleSpecRejectsTooManyStatements(t *testing.T) {
	statements := make([]interface{}, 51)
	for i := range statements {
		statements[i] = minimalStatement()
	}

	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{
			"Version":   "2012-10-17",
			"Statement": statements,
		},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected 51 statements to be rejected by MaxItems=50, got no errors")
	}

	if !containsErr(errs, "50") {
		t.Fatalf("expected error mentioning the 50-item cap, got %v", errs)
	}
}

func TestRoleSpecRejectsStatementWithTooManyTopLevelKeys(t *testing.T) {
	// Build a single statement with 17 top-level keys to trip MaxProperties=16.
	// IAM-shaped keys are not required because XPreserveUnknownFields lets the
	// statement carry arbitrary keys; only the count is bounded.
	wideStatement := make(map[string]interface{}, 17)
	for i := 0; i < 17; i++ {
		// Use distinct keys; keys themselves have no length cap on
		// XPreserveUnknownFields, only the property count matters.
		wideStatement["Key"+stringOfDigit(i)] = "v"
	}

	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{
			"Version": "2012-10-17",
			"Statement": []interface{}{
				wideStatement,
			},
		},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected a statement with 17 top-level keys to be rejected by MaxProperties=16, got no errors")
	}

	if !containsErr(errs, "16") {
		t.Fatalf("expected error mentioning the 16-property cap, got %v", errs)
	}
}

func TestRoleSpecAcceptsMaxLengthID(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{
			"Version": "2012-10-17",
			"Id":      strings.Repeat("a", 128),
			"Statement": []interface{}{
				minimalStatement(),
			},
		},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) != 0 {
		t.Fatalf("expected Id at MaxLength=128 to be accepted, got %v", errs)
	}
}

func TestRoleSpecRejectsOverMaxLengthID(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{
			"Version": "2012-10-17",
			"Id":      strings.Repeat("a", 129),
			"Statement": []interface{}{
				minimalStatement(),
			},
		},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected Id at length 129 to be rejected by MaxLength=128, got no errors")
	}

	if !containsErr(errs, "128") {
		t.Fatalf("expected error mentioning the 128-char cap, got %v", errs)
	}
}

func TestRoleSpecRejectsMissingPolicyARNsAndDocument(t *testing.T) {
	// Sanity guard: the existing top-level XValidation rule
	// `(has(self.policyARNs) && self.policyARNs.size() > 0) || has(self.policyDocument)`
	// must still reject specs that supply neither field. This protects against
	// regressions where an API schema validation refactor silently drops the rule.
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

// stringOfDigit returns a deterministic short suffix used to build distinct
// keys when stress-testing MaxProperties bounds. Kept local to avoid pulling
// in fmt.Sprintf inside a tight loop and to keep the test self-contained.
func stringOfDigit(i int) string {
	// Two-digit suffix is sufficient for the small (<=17) cases in this file.
	const digits = "0123456789"
	if i < 10 {
		return string(digits[i])
	}

	return string(digits[i/10]) + string(digits[i%10])
}
