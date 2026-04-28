package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestDeliveryPredicates(t *testing.T) {
	for _, delivery := range []identityv1.DeliveryType{
		identityv1.DeliveryTypeSelfHostedIRSA,
		identityv1.DeliveryTypeEKSIRSA,
	} {
		if !delivery.UsesAnnotationBasedIRSA() {
			t.Fatalf("%s should be annotation-based IRSA", delivery)
		}
	}

	if identityv1.DeliveryTypeEKSPodIdentity.UsesAnnotationBasedIRSA() {
		t.Fatal("EKSPodIdentity must stay outside annotation-based IRSA")
	}
}

func TestUsesManagedConfigOIDCProvider(t *testing.T) {
	if usesManagedConfigOIDCProvider(nil) {
		t.Fatal("nil config must not use a managed IAM OIDC provider")
	}

	config := &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{Name: identityv1.DefaultName, Namespace: "wlc-a"},
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type: identityv1.DeliveryTypeEKSIRSA,
			EKSIRSA: &identityv1.EKSIRSAConfig{
				OIDCProvider: identityv1.EKSIRSAOIDCProviderConfig{
					Management: identityv1.OIDCProviderManagementManaged,
				},
			},
		},
	}

	config.Spec.Type = identityv1.DeliveryTypeSelfHostedIRSA
	if usesManagedConfigOIDCProvider(config) {
		t.Fatal("non-EKSIRSA config must not use a managed IAM OIDC provider")
	}

	config.Spec.Type = identityv1.DeliveryTypeEKSIRSA
	config.Spec.EKSIRSA = nil
	if usesManagedConfigOIDCProvider(config) {
		t.Fatal("EKSIRSA config with nil spec.eksIRSA must not use a managed IAM OIDC provider")
	}

	config.Spec.EKSIRSA = &identityv1.EKSIRSAConfig{
		OIDCProvider: identityv1.EKSIRSAOIDCProviderConfig{
			Management: identityv1.OIDCProviderManagementManaged,
		},
	}
	if !usesManagedConfigOIDCProvider(config) {
		t.Fatal("managed EKSIRSA config should use a managed IAM OIDC provider")
	}

	config.Spec.EKSIRSA.OIDCProvider.Management = identityv1.OIDCProviderManagementExternal
	config.Spec.EKSIRSA.OIDCProvider.ARN = "arn:aws:iam::123456789012:oidc-provider/example.com"
	if usesManagedConfigOIDCProvider(config) {
		t.Fatal("external EKSIRSA config must not use a managed IAM OIDC provider")
	}
}
