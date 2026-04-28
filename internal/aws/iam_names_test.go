package aws

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestLabelsForConfigAreValidKubernetesLabels(t *testing.T) {
	config := &identityv1.AWSWorkloadIdentityConfig{}
	config.Namespace = "wlc-20260428081433"
	config.Name = "default"
	config.UID = types.UID("cd71abe8-c4d0-41b2-8bb8-d6a872c1ad0f")
	config.Spec.Type = identityv1.DeliveryTypeSelfHostedIRSA

	for key, value := range LabelsForConfig(config) {
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			t.Fatalf("label %s=%q is invalid: %v", key, value, errs)
		}
	}
}

func TestLabelsForRoleAreValidKubernetesLabels(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{}
	role.Namespace = "very-long-workload-cluster-namespace-that-needs-to-fit-in-labels"
	role.Name = "sts-canary"
	role.UID = types.UID("204381cb-58f0-47a9-ac3d-0f75cb1d293d")
	role.Spec.ServiceAccount.Namespace = "very-long-application-namespace-that-needs-to-fit-in-labels"
	role.Spec.ServiceAccount.Name = "very-long-service-account-name-that-needs-to-fit-in-labels"

	for key, value := range LabelsForRole(role, identityv1.DeliveryTypeSelfHostedIRSA) {
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			t.Fatalf("label %s=%q is invalid: %v", key, value, errs)
		}
	}
}
