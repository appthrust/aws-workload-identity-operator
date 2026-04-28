package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const operatorConfigCRDPath = "../../config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml"

// loadOperatorConfigSpecSchema reads the rendered OperatorConfig CRD and
// returns its spec sub-schema.
func loadOperatorConfigSpecSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	abs, err := filepath.Abs(operatorConfigCRDPath)
	if err != nil {
		t.Fatalf("resolve CRD path: %v", err)
	}
	// G304: operatorConfigCRDPath is a compile-time const pointing at the
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

	specSchema, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		t.Fatalf("CRD root has no spec property")
	}

	return specSchema
}

// TestSelfHostedIRSADefaultsCascadeWhenParentOmitted locks in the contract
// that the apiserver materializes spec.selfHostedIRSA as an empty object when
// users omit the field, so the inner webhookNamespace default is filled in by
// schema-level defaulting rather than by Go-side fallback. The field is
// declared as a non-pointer struct with omitempty, which without an
// object-level default leaves the stored value as a missing object and
// prevents the kube-apiserver from descending into properties.webhookNamespace
// to apply its default. The +kubebuilder:default={} marker on the struct
// field is what makes the cascade work; this test guards against accidental
// removal of that marker.
func TestSelfHostedIRSADefaultsCascadeWhenParentOmitted(t *testing.T) {
	spec := loadOperatorConfigSpecSchema(t)

	selfHostedIRSA, ok := spec.Properties["selfHostedIRSA"]
	if !ok {
		t.Fatalf("spec has no selfHostedIRSA property")
	}

	if selfHostedIRSA.Default == nil {
		t.Fatalf("selfHostedIRSA has no default; inner webhookNamespace default will not cascade when the parent is omitted")
	}

	if got := strings.TrimSpace(string(selfHostedIRSA.Default.Raw)); got != "{}" {
		t.Fatalf("selfHostedIRSA default = %q, want %q so the apiserver materializes an empty object and inner defaults fire", got, "{}")
	}

	webhookNamespace, ok := selfHostedIRSA.Properties["webhookNamespace"]
	if !ok {
		t.Fatalf("selfHostedIRSA has no webhookNamespace property")
	}

	if webhookNamespace.Default == nil {
		t.Fatalf("webhookNamespace has no default; the cascade has nothing to fill in")
	}

	if got := strings.Trim(string(webhookNamespace.Default.Raw), `"`); got != "aws-pod-identity-webhook" {
		t.Fatalf("webhookNamespace default = %q, want %q", got, "aws-pod-identity-webhook")
	}
}
