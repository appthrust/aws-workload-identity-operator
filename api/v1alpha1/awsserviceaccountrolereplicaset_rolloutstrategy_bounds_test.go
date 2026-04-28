// Package v1alpha1_test contains regression coverage for the CEL XValidation
// rules attached to PlacementRef that bound the embedded OCM RolloutStrategy.
// The upstream OCM type does not declare maxItems on mandatoryDecisionGroups,
// so this CRD adds a local list-size cap to satisfy api-conventions.md
// guidance that list fields are size-checked.
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

// loadPlacementRefItemSchema returns the internal JSONSchemaProps for one
// element of spec.placementRefs (i.e. a single PlacementRef). The PlacementRef
// schema is where the rolloutStrategy bounds live as item-level
// x-kubernetes-validations rules, so tests can validate a single PlacementRef
// payload against this sub-schema.
func loadPlacementRefItemSchema(t *testing.T) *apiextensionsinternal.JSONSchemaProps {
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

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	specSchema, ok := root.Properties["spec"]
	if !ok {
		t.Fatalf("CRD root has no spec property")
	}

	placementRefsSchema, ok := specSchema.Properties["placementRefs"]
	if !ok {
		t.Fatalf("CRD spec has no placementRefs property")
	}

	if placementRefsSchema.Items == nil || placementRefsSchema.Items.Schema == nil {
		t.Fatalf("placementRefs has no items schema")
	}

	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(placementRefsSchema.Items.Schema, internal, nil); err != nil {
		t.Fatalf("convert placementRefs item schema to internal: %v", err)
	}

	return internal
}

func validatePlacementRef(t *testing.T, obj map[string]interface{}) field.ErrorList {
	t.Helper()

	internalSchema := loadPlacementRefItemSchema(t)

	openAPIValidator, _, err := apiservervalidation.NewSchemaValidator(internalSchema)
	if err != nil {
		t.Fatalf("build openapi validator: %v", err)
	}

	errs := apiservervalidation.ValidateCustomResource(field.NewPath("placementRef"), obj, openAPIValidator)

	structural, err := schema.NewStructural(internalSchema)
	if err != nil {
		t.Fatalf("build structural schema: %v", err)
	}

	celValidator := celschema.NewValidator(structural, false, celconfig.PerCallLimit)
	if celValidator != nil {
		celErrs, _ := celValidator.Validate(context.Background(), field.NewPath("placementRef"), structural, obj, nil, celconfig.RuntimeCELCostBudget)
		errs = append(errs, celErrs...)
	}

	return errs
}

func TestPlacementRefAcceptsRolloutStrategyWithinBounds(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]interface{}
	}{
		{
			name: "default rolloutStrategy (type=All)",
			obj: map[string]interface{}{
				"name": "production",
				"rolloutStrategy": map[string]interface{}{
					"type": "All",
				},
			},
		},
		{
			name: "progressive with small mandatoryDecisionGroups",
			obj: map[string]interface{}{
				"name": "production",
				"rolloutStrategy": map[string]interface{}{
					"type": "Progressive",
					"progressive": map[string]interface{}{
						"mandatoryDecisionGroups": []interface{}{
							map[string]interface{}{"groupName": "tier-0"},
							map[string]interface{}{"groupName": "tier-1"},
						},
					},
				},
			},
		},
		{
			name: "progressivePerGroup with mandatoryDecisionGroups at the 32-entry boundary",
			obj: map[string]interface{}{
				"name": "production",
				"rolloutStrategy": map[string]interface{}{
					"type": "ProgressivePerGroup",
					"progressivePerGroup": map[string]interface{}{
						"mandatoryDecisionGroups": make32GroupIndexEntries(),
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validatePlacementRef(t, tc.obj)
			if len(errs) != 0 {
				t.Fatalf("expected no validation errors, got %v", errs)
			}
		})
	}
}

func make32GroupIndexEntries() []interface{} {
	entries := make([]interface{}, 0, 32)
	for i := 0; i < 32; i++ {
		entries = append(entries, map[string]interface{}{"groupIndex": int64(i)})
	}

	return entries
}

func TestPlacementRefRejectsTooManyMandatoryDecisionGroups(t *testing.T) {
	cases := []struct {
		strategyType  string
		strategyField string
		wantInErrPart string
	}{
		{
			strategyType:  "Progressive",
			strategyField: "progressive",
			wantInErrPart: "rolloutStrategy.progressive.mandatoryDecisionGroups must have at most 32 entries",
		},
		{
			strategyType:  "ProgressivePerGroup",
			strategyField: "progressivePerGroup",
			wantInErrPart: "rolloutStrategy.progressivePerGroup.mandatoryDecisionGroups must have at most 32 entries",
		},
	}

	for _, tc := range cases {
		t.Run(tc.strategyType, func(t *testing.T) {
			groups := make([]interface{}, 0, 33)
			for i := 0; i < 33; i++ {
				groups = append(groups, map[string]interface{}{"groupIndex": int64(i)})
			}

			obj := map[string]interface{}{
				"name": "production",
				"rolloutStrategy": map[string]interface{}{
					"type": tc.strategyType,
					tc.strategyField: map[string]interface{}{
						"mandatoryDecisionGroups": groups,
					},
				},
			}

			errs := validatePlacementRef(t, obj)

			if len(errs) == 0 {
				t.Fatalf("expected size cap to reject 33 entries, got no errors")
			}

			if !containsErr(errs, tc.wantInErrPart) {
				t.Fatalf("expected error containing %q, got %v", tc.wantInErrPart, errs)
			}
		})
	}
}
