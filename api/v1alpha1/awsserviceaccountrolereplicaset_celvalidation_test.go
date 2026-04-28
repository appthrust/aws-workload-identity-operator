// Package v1alpha1_test contains regression coverage for the CRD-level CEL
// XValidation rules attached to TemplateMetadata. These tests evaluate the
// generated CRD schema directly (no envtest required) by:
//
//  1. Parsing config/crd/bases/aws.identity.appthrust.io_awsserviceaccountrolereplicasets.yaml.
//  2. Extracting the spec.template.metadata sub-schema.
//  3. Converting it to a structural schema and running both the OpenAPI
//     validator (for keywords like maxProperties) and the CEL validator
//     (for x-kubernetes-validations rules) against representative inputs.
//
// The CRD YAML is the contract that the apiserver enforces, so testing
// against the rendered schema gives the same coverage as a real
// kube-apiserver Create() round-trip without spinning up etcd/envtest.
package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

const replicaSetCRDPath = "../../config/crd/bases/aws.identity.appthrust.io_awsserviceaccountrolereplicasets.yaml"

// loadTemplateMetadataSchema reads the rendered CRD and returns the
// spec.template.metadata sub-schema converted to the internal apiextensions
// representation. The internal representation is required by both the
// structural-schema constructor and the OpenAPI SchemaValidator.
func loadTemplateMetadataSchema(t *testing.T) *apiextensionsinternal.JSONSchemaProps {
	t.Helper()

	abs, err := filepath.Abs(replicaSetCRDPath)
	if err != nil {
		t.Fatalf("resolve CRD path: %v", err)
	}
	// G304: replicaSetCRDPath is a compile-time const pointing at the
	// generated CRD inside this repo; this test cannot be reached with an
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

	metadataSchema := navigateToTemplateMetadata(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)

	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&metadataSchema, internal, nil); err != nil {
		t.Fatalf("convert metadata schema to internal: %v", err)
	}

	return internal
}

func navigateToTemplateMetadata(t *testing.T, root *apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	specSchema, ok := root.Properties["spec"]
	if !ok {
		t.Fatalf("CRD root has no spec property")
	}

	templateSchema, ok := specSchema.Properties["template"]
	if !ok {
		t.Fatalf("CRD spec has no template property")
	}

	metadataSchema, ok := templateSchema.Properties["metadata"]
	if !ok {
		t.Fatalf("CRD spec.template has no metadata property")
	}

	return metadataSchema
}

// validateTemplateMetadata runs the same OpenAPI + CEL validation pipeline that
// the kube-apiserver applies during Create/Update on the spec.template.metadata
// field. Returns the combined field errors so tests can inspect both the
// OpenAPI structural keywords (maxProperties) and the CEL XValidation rules.
func validateTemplateMetadata(t *testing.T, obj map[string]interface{}) field.ErrorList {
	t.Helper()

	internalSchema := loadTemplateMetadataSchema(t)

	// OpenAPI validator covers keywords like maxProperties.
	openAPIValidator, _, err := apiservervalidation.NewSchemaValidator(internalSchema)
	if err != nil {
		t.Fatalf("build openapi validator: %v", err)
	}

	errs := apiservervalidation.ValidateCustomResource(field.NewPath("metadata"), obj, openAPIValidator)

	// CEL validator covers x-kubernetes-validations.
	structural, err := schema.NewStructural(internalSchema)
	if err != nil {
		t.Fatalf("build structural schema: %v", err)
	}

	celValidator := celschema.NewValidator(structural, false, celconfig.PerCallLimit)
	if celValidator != nil {
		celErrs, _ := celValidator.Validate(context.Background(), field.NewPath("metadata"), structural, obj, nil, celconfig.RuntimeCELCostBudget)
		errs = append(errs, celErrs...)
	}

	return errs
}

func TestTemplateMetadataCELAcceptsValidLabelsAndAnnotations(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]interface{}
	}{
		{
			name: "prefixed standard labels and annotations",
			obj: map[string]interface{}{
				"labels": map[string]interface{}{
					"app.kubernetes.io/name":      "my-app",
					"app.kubernetes.io/component": "worker",
				},
				"annotations": map[string]interface{}{
					"example.com/note":  "arbitrary <text>, including : and spaces",
					"example.com/owner": "platform-team",
				},
			},
		},
		{
			name: "label value may be empty",
			obj: map[string]interface{}{
				"labels": map[string]interface{}{
					"example.com/marker": "",
				},
			},
		},
		{
			name: "unprefixed label key",
			obj: map[string]interface{}{
				"labels": map[string]interface{}{
					"app": "demo",
				},
			},
		},
		{
			name: "empty maps",
			obj: map[string]interface{}{
				"labels":      map[string]interface{}{},
				"annotations": map[string]interface{}{},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateTemplateMetadata(t, tc.obj)
			if len(errs) != 0 {
				t.Fatalf("expected no validation errors, got %v", errs)
			}
		})
	}
}

// invalidLabelCases collects representative label-shape inputs that the CRD
// must reject. The cases are kept in a package-level slice so the matching
// test function stays short enough for the funlen linter.
//
//nolint:gochecknoglobals // immutable table-test data shared with one test
var invalidLabelCases = []struct {
	name      string
	obj       map[string]interface{}
	wantInErr string
}{
	{
		name: "label key with multiple slashes",
		obj: map[string]interface{}{
			"labels": map[string]interface{}{
				"Foo/Bar/Baz": "value",
			},
		},
		wantInErr: "label keys must be valid Kubernetes qualified names",
	},
	{
		name: "empty label key",
		obj: map[string]interface{}{
			"labels": map[string]interface{}{
				"": "value",
			},
		},
		wantInErr: "label keys must be valid Kubernetes qualified names",
	},
	{
		name: "label key with illegal whitespace",
		obj: map[string]interface{}{
			"labels": map[string]interface{}{
				"foo bar": "value",
			},
		},
		wantInErr: "label keys must be valid Kubernetes qualified names",
	},
	{
		name: "label value exceeding 63 chars",
		obj: map[string]interface{}{
			"labels": map[string]interface{}{
				"example.com/name": strings.Repeat("a", 64),
			},
		},
		wantInErr: "63",
	},
	{
		name: "label value with illegal character",
		obj: map[string]interface{}{
			"labels": map[string]interface{}{
				"example.com/name": "bad value with space",
			},
		},
		wantInErr: "should match",
	},
}

func TestTemplateMetadataCELRejectsInvalidLabels(t *testing.T) {
	for _, tc := range invalidLabelCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateTemplateMetadata(t, tc.obj)

			if len(errs) == 0 {
				t.Fatalf("expected validation error containing %q, got none", tc.wantInErr)
			}

			if !containsErr(errs, tc.wantInErr) {
				t.Fatalf("expected an error containing %q, got %v", tc.wantInErr, errs)
			}
		})
	}
}

func TestTemplateMetadataCELRejectsInvalidAnnotations(t *testing.T) {
	cases := []struct {
		name      string
		obj       map[string]interface{}
		wantInErr string
	}{
		{
			name: "annotation key with multiple slashes",
			obj: map[string]interface{}{
				"annotations": map[string]interface{}{
					"foo/bar/baz": "value",
				},
			},
			wantInErr: "annotation keys must be valid Kubernetes qualified names",
		},
		{
			name: "empty annotation key",
			obj: map[string]interface{}{
				"annotations": map[string]interface{}{
					"": "value",
				},
			},
			wantInErr: "annotation keys must be valid Kubernetes qualified names",
		},
		{
			name: "annotation key with illegal character",
			obj: map[string]interface{}{
				"annotations": map[string]interface{}{
					"foo bar": "value",
				},
			},
			wantInErr: "annotation keys must be valid Kubernetes qualified names",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateTemplateMetadata(t, tc.obj)

			if len(errs) == 0 {
				t.Fatalf("expected validation error containing %q, got none", tc.wantInErr)
			}

			if !containsErr(errs, tc.wantInErr) {
				t.Fatalf("expected an error containing %q, got %v", tc.wantInErr, errs)
			}
		})
	}
}

// reservedTemplateLabelKeys mirrors the reserved label set enforced by both the
// CRD CEL rule on TemplateMetadata.Labels and the controller-side
// validateReplicaSetTemplateMetadata helper. The CEL rule rejects these keys at
// admission, before any controller observes the object; the controller-side
// check is a self-defense for resources that predate the rule.
//
//nolint:gochecknoglobals // immutable table-test data shared with one test
var reservedTemplateLabelKeys = []string{
	"app.kubernetes.io/managed-by",
	"aws.identity.appthrust.io/binding-uid",
	"aws.identity.appthrust.io/config-uid",
	"aws.identity.appthrust.io/delivery",
	"aws.identity.appthrust.io/inventory-namespace",
	"aws.identity.appthrust.io/owner-ref",
	"aws.identity.appthrust.io/replicaset-uid",
	"aws.identity.appthrust.io/runtime",
	"aws.identity.appthrust.io/service-account",
}

func TestTemplateMetadataCELRejectsReservedLabels(t *testing.T) {
	for _, key := range reservedTemplateLabelKeys {
		t.Run(key, func(t *testing.T) {
			errs := validateTemplateMetadata(t, map[string]interface{}{
				"labels": map[string]interface{}{
					key: "value",
				},
			})

			if len(errs) == 0 {
				t.Fatalf("expected reserved label %q to be rejected, got no errors", key)
			}

			if !containsErr(errs, "label key is reserved by aws-workload-identity-operator") {
				t.Fatalf("expected reserved-label error for %q, got %v", key, errs)
			}
		})
	}
}

func TestTemplateMetadataCELRejectsReservedAnnotation(t *testing.T) {
	errs := validateTemplateMetadata(t, map[string]interface{}{
		"annotations": map[string]interface{}{
			"aws.identity.appthrust.io/replicaset-owner-ref": "value",
		},
	})

	if len(errs) == 0 {
		t.Fatalf("expected reserved annotation to be rejected, got no errors")
	}

	if !containsErr(errs, "is reserved by aws-workload-identity-operator") {
		t.Fatalf("expected reserved-annotation error, got %v", errs)
	}
}

// TestTemplateMetadataCELAcceptsNonReservedOperatorPrefixedKey confirms the
// reserved-key rule does not over-reject every aws.identity.appthrust.io key —
// only the specific keys the operator stamps onto generated children.
func TestTemplateMetadataCELAcceptsNonReservedOperatorPrefixedKey(t *testing.T) {
	errs := validateTemplateMetadata(t, map[string]interface{}{
		"labels": map[string]interface{}{
			"aws.identity.appthrust.io/example": "ok",
		},
		"annotations": map[string]interface{}{
			"aws.identity.appthrust.io/example": "ok",
		},
	})

	if len(errs) != 0 {
		t.Fatalf("expected non-reserved operator-prefixed keys to be accepted, got %v", errs)
	}
}

func TestTemplateMetadataRejectsTooManyLabels(t *testing.T) {
	labels := make(map[string]interface{}, 65)
	for i := 0; i < 65; i++ {
		labels[fmt.Sprintf("example.com/label-%d", i)] = "v"
	}

	errs := validateTemplateMetadata(t, map[string]interface{}{
		"labels": labels,
	})

	if len(errs) == 0 {
		t.Fatalf("expected maxProperties=64 to reject 65 labels, got no errors")
	}
	// kube-openapi surfaces this as a "must have at most 64 properties" message.
	if !containsErr(errs, "64") {
		t.Fatalf("expected error mentioning the 64-property cap, got %v", errs)
	}
}

func TestTemplateMetadataRejectsTooManyAnnotations(t *testing.T) {
	annotations := make(map[string]interface{}, 65)
	for i := 0; i < 65; i++ {
		annotations[fmt.Sprintf("example.com/ann-%d", i)] = "v"
	}

	errs := validateTemplateMetadata(t, map[string]interface{}{
		"annotations": annotations,
	})

	if len(errs) == 0 {
		t.Fatalf("expected maxProperties=64 to reject 65 annotations, got no errors")
	}

	if !containsErr(errs, "64") {
		t.Fatalf("expected error mentioning the 64-property cap, got %v", errs)
	}
}

// TestTemplateMetadataRejectsTooLongAnnotationValue ensures the per-value
// MaxLength bound on annotation values (mirrored from apimachinery's
// TotalAnnotationSizeLimitB) is wired through to the rendered CRD schema, so
// the apiserver rejects pathologically large single annotation payloads.
func TestTemplateMetadataRejectsTooLongAnnotationValue(t *testing.T) {
	const maxAnnotationValueLen = 262144

	errs := validateTemplateMetadata(t, map[string]interface{}{
		"annotations": map[string]interface{}{
			"example.com/big": strings.Repeat("x", maxAnnotationValueLen+1),
		},
	})

	if len(errs) == 0 {
		t.Fatalf("expected maxLength=%d to reject an oversized annotation value, got no errors", maxAnnotationValueLen)
	}

	if !containsErr(errs, fmt.Sprintf("%d", maxAnnotationValueLen)) {
		t.Fatalf("expected error mentioning the %d-byte cap, got %v", maxAnnotationValueLen, errs)
	}
}

func TestReplicaSetCRDStaysWithinStaticCELCostBudget(t *testing.T) {
	abs, err := filepath.Abs(replicaSetCRDPath)
	if err != nil {
		t.Fatalf("resolve CRD path: %v", err)
	}
	raw, err := os.ReadFile(abs) //nolint:gosec // const path inside repo tree
	if err != nil {
		t.Fatalf("read CRD %s: %v", abs, err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096).Decode(&crd); err != nil {
		t.Fatalf("decode CRD: %v", err)
	}

	internal := &apiextensionsinternal.CustomResourceDefinition{}
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, internal, nil); err != nil {
		t.Fatalf("convert CRD to internal: %v", err)
	}
	internal.Status.StoredVersions = []string{"v1alpha1"}

	errs := apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), internal)
	if len(errs) != 0 {
		t.Fatalf("expected CRD to pass Kubernetes static validation, got %v", errs)
	}
}

func containsErr(errs field.ErrorList, needle string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), needle) {
			return true
		}
	}

	return false
}
