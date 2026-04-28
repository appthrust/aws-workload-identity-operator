package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// loadCRDStatusSchema reads the rendered CRD at crdPath and returns the
// status sub-schema. crdPath is expected to be one of the *CRDPath constants
// defined alongside each per-CRD celvalidation test.
func loadCRDStatusSchema(t *testing.T, crdPath string) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	abs, err := filepath.Abs(crdPath)
	if err != nil {
		t.Fatalf("resolve CRD path: %v", err)
	}
	// G304: crdPath is a compile-time const pointing at the generated CRD
	// inside this repo; this test cannot be reached with an
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

	statusSchema, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	if !ok {
		t.Fatalf("CRD root has no status property")
	}

	return statusSchema
}
