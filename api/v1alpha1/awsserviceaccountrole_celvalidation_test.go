// Package v1alpha1_test contains regression coverage for the CRD schema
// applied to the AWSServiceAccountRole spec.policyDocument object field and
// for the spec-level XValidation rule that requires exactly one of
// spec.policyARNs / spec.policyDocument.
//
// The tests evaluate the generated CRD schema directly (no envtest) by:
//
//  1. Parsing config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml.
//  2. Extracting the spec sub-schema.
//  3. Converting to a structural schema and running the OpenAPI validator
//     (for type checks) plus the CEL validator
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

// minimalPolicyDocument returns a well-formed IAM policy JSON object.
func minimalPolicyDocument() map[string]interface{} {
	return map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []interface{}{
			map[string]interface{}{
				"Effect":   "Allow",
				"Action":   []interface{}{"s3:GetObject"},
				"Resource": "arn:aws:s3:::example-bucket/*",
			},
		},
	}
}

func TestRoleSpecAcceptsValidIAMPolicyDocument(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": minimalPolicyDocument(),
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) != 0 {
		t.Fatalf("expected valid IAM policy document object to be accepted, got %v", errs)
	}
}

func TestRoleSpecRejectsStringPolicyDocument(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": `{"Version":"2012-10-17","Statement":[]}`,
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected string policyDocument to be rejected, got no errors")
	}

	if !containsErr(errs, "object") {
		t.Fatalf("expected object type rejection, got %v", errs)
	}
}

func TestRoleSpecRejectsArrayPolicyDocument(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": []interface{}{minimalPolicyDocument()},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected array policyDocument to be rejected, got no errors")
	}

	if !containsErr(errs, "object") {
		t.Fatalf("expected object type rejection, got %v", errs)
	}
}

func TestRoleSpecRejectsEmptyPolicyDocumentObject(t *testing.T) {
	spec := map[string]interface{}{
		"serviceAccount": validServiceAccount(),
		"policyDocument": map[string]interface{}{},
	}

	errs := validateRoleSpec(t, spec)
	if len(errs) == 0 {
		t.Fatalf("expected empty policyDocument object to be rejected, got no errors")
	}

	if !containsErr(errs, "at least 1") && !containsErr(errs, "1 properties") {
		t.Fatalf("expected minProperties rejection, got %v", errs)
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
