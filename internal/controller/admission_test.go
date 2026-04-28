package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

var errAdmissionFieldSelectorUsed = errors.New("field selector used")

func TestConfigValidatorBlocksDeleteWhenRolesRemain(t *testing.T) {
	config := testAdmissionConfig(nil)
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: config.Namespace},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
		},
	}
	validator := ConfigValidator{Client: testConfigClient(t, role)}

	if _, err := validator.ValidateDelete(context.Background(), config); err == nil {
		t.Fatal("expected delete to be blocked")
	}
}

func TestConfigValidatorAllowsForceDeleteWhenRolesRemain(t *testing.T) {
	config := testAdmissionConfig(map[string]string{identityv1.ForceDeleteAnnotation: "true"})
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: config.Namespace},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
		},
	}
	validator := ConfigValidator{Client: testConfigClient(t, role)}

	if _, err := validator.ValidateDelete(context.Background(), config); err != nil {
		t.Fatal(err)
	}
}

func TestRoleValidatorBlocksDuplicateServiceAccountBinding(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "app")
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateCreate(context.Background(), candidate); err == nil {
		t.Fatal("expected duplicate ServiceAccount binding to be blocked")
	}
}

func TestRoleValidatorDoesNotRequireFieldSelectableCRD(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "app")
	validator := RoleValidator{Client: noFieldSelectorReader{Reader: testConfigClient(t, config, existing)}}

	_, err := validator.ValidateCreate(context.Background(), candidate)
	if err == nil {
		t.Fatal("expected duplicate ServiceAccount binding to be blocked")
	}

	if errors.Is(err, errAdmissionFieldSelectorUsed) {
		t.Fatal("expected admission validation to list roles without a field selector")
	}

	if !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("expected duplicate binding error, got %v", err)
	}
}

func TestRoleValidatorAllowsDifferentServiceAccountBinding(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "other")
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateCreate(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
}

func TestRoleValidatorAllowsSelfUpdate(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	existing.UID = types.UID(testRoleUID)
	updated := existing.DeepCopy()
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateUpdate(context.Background(), existing, updated); err != nil {
		t.Fatal(err)
	}
}

func testAdmissionConfig(annotations map[string]string) *identityv1.AWSWorkloadIdentityConfig {
	return &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:        identityv1.DefaultName,
			Namespace:   testInventoryNamespace,
			Annotations: annotations,
		},
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type:   identityv1.DeliveryTypeSelfHostedIRSA,
			Region: "ap-northeast-1",
		},
	}
}

type noFieldSelectorReader struct {
	client.Reader
}

func (r noFieldSelectorReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	listOptions := (&client.ListOptions{}).ApplyOptions(opts)
	if listOptions.FieldSelector != nil && !listOptions.FieldSelector.Empty() {
		return errAdmissionFieldSelectorUsed
	}

	if listOptions.Raw != nil && listOptions.Raw.FieldSelector != "" {
		return errAdmissionFieldSelectorUsed
	}

	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("noFieldSelectorReader List: %w", err)
	}

	return nil
}
