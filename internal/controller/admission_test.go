package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

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
