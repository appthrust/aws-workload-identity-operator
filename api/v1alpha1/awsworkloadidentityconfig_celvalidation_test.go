// Package v1alpha1_test contains regression coverage for the OpenAPI
// keyword and CEL XValidation rules attached to the AWSWorkloadIdentityConfig
// CRD. These tests evaluate the generated CRD schema directly (no envtest
// required) by:
//
//  1. Parsing config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml.
//  2. Extracting the spec.region leaf sub-schema (a scalar string carrying
//     MinLength / MaxLength / Pattern markers plus an immutability
//     x-kubernetes-validations rule).
//  3. Converting it to a structural schema and running the OpenAPI validator
//     (for MinLength / MaxLength / Pattern) against representative string
//     inputs. The immutability XValidation needs `oldSelf` so it is exercised
//     elsewhere (integration / envtest) and intentionally skipped here.
//
// The CRD YAML is the contract that the apiserver enforces, so testing
// against the rendered schema gives the same coverage as a real
// kube-apiserver Create() round-trip without spinning up etcd/envtest.
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

const workloadIdentityConfigCRDPath = "../../config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml"

// loadRegionSchema reads the rendered CRD and returns the spec.region leaf
// sub-schema converted to the internal apiextensions representation. The
// internal representation is required by the OpenAPI SchemaValidator.
func loadRegionSchema(t *testing.T) *apiextensionsinternal.JSONSchemaProps {
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

	regionSchema := navigateToRegion(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)

	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&regionSchema, internal, nil); err != nil {
		t.Fatalf("convert region schema to internal: %v", err)
	}

	return internal
}

func navigateToRegion(t *testing.T, root *apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	specSchema, ok := root.Properties["spec"]
	if !ok {
		t.Fatalf("CRD root has no spec property")
	}

	regionSchema, ok := specSchema.Properties["region"]
	if !ok {
		t.Fatalf("CRD spec has no region property")
	}

	return regionSchema
}

// validateRegion runs the OpenAPI validation pipeline that the kube-apiserver
// applies to spec.region during Create/Update. spec.region is a leaf string
// schema, so the scalar value is passed directly as the customResource
// argument to ValidateCustomResource. The CEL immutability rule
// (self == oldSelf) requires a prior oldSelf and is exercised by integration
// coverage, not by this OpenAPI-only test.
func validateRegion(t *testing.T, value string) field.ErrorList {
	t.Helper()

	internalSchema := loadRegionSchema(t)

	openAPIValidator, _, err := apiservervalidation.NewSchemaValidator(internalSchema)
	if err != nil {
		t.Fatalf("build openapi validator: %v", err)
	}

	return apiservervalidation.ValidateCustomResource(field.NewPath("spec", "region"), value, openAPIValidator)
}

func TestRegionAcceptsAllPartitions(t *testing.T) {
	cases := []string{
		"us-east-1",
		"ap-northeast-1",
		"us-gov-east-1",
		"cn-north-1",
		"us-iso-east-1",
		"us-isob-east-1",
		"eu-isoe-west-1",
		"eusc-de-east-1",
	}

	for _, region := range cases {
		t.Run(region, func(t *testing.T) {
			errs := validateRegion(t, region)
			if len(errs) != 0 {
				t.Fatalf("expected region %q to be accepted, got %v", region, errs)
			}
		})
	}
}

func TestRegionRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{
			name:  "empty rejected by MinLength",
			value: "",
		},
		{
			name:  "uppercase rejected by Pattern",
			value: "US-EAST-1",
		},
		{
			name:  "whitespace rejected by Pattern",
			value: "us east 1",
		},
		{
			name:  "underscore rejected by Pattern",
			value: "us_east_1",
		},
		{
			name:  "missing trailing digit rejected by Pattern",
			value: "us-east",
		},
		{
			name:  "leading digit rejected by Pattern",
			value: "1us-east-1",
		},
		{
			// 33 'a' chars in the first label keeps the overall shape
			// pattern-valid (lowercase letters, hyphens, trailing digit
			// segment), so MaxLength=32 is the only marker that should
			// reject it.
			name:  "exceeds MaxLength",
			value: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-east-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateRegion(t, tc.value)
			if len(errs) == 0 {
				t.Fatalf("expected region %q to be rejected, got no errors", tc.value)
			}
		})
	}
}
