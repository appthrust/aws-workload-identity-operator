package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestIndexAWSServiceAccountRoleBySA(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
		},
	}

	got := IndexAWSServiceAccountRoleBySA(role)
	if want := []string{"default/app"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestIndexAWSServiceAccountRoleBySASkipsIncompleteOrWrongType(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
	}{
		{
			name: "empty namespace",
			obj: &identityv1.AWSServiceAccountRole{
				Spec: identityv1.AWSServiceAccountRoleSpec{
					ServiceAccount: identityv1.ServiceAccountSubject{Name: "app"},
				},
			},
		},
		{
			name: "empty name",
			obj: &identityv1.AWSServiceAccountRole{
				Spec: identityv1.AWSServiceAccountRoleSpec{
					ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default"},
				},
			},
		},
		{
			name: "wrong type",
			obj:  &corev1.ServiceAccount{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IndexAWSServiceAccountRoleBySA(tt.obj); got != nil {
				t.Fatalf("expected nil index, got %v", got)
			}
		})
	}
}

func TestIndexAWSWorkloadIdentityConfigByResolvedCluster(t *testing.T) {
	config := &identityv1.AWSWorkloadIdentityConfig{
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type: identityv1.DeliveryTypeSelfHostedIRSA,
		},
		Status: identityv1.AWSWorkloadIdentityConfigStatus{
			ResolvedClusterName: testResolvedClusterName,
		},
	}

	got := IndexAWSWorkloadIdentityConfigByResolvedCluster(config)
	if want := []string{testResolvedClusterName}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestIndexAWSWorkloadIdentityConfigByResolvedClusterSkipsInvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
	}{
		{
			name: "empty resolved cluster",
			obj: &identityv1.AWSWorkloadIdentityConfig{
				Spec: identityv1.AWSWorkloadIdentityConfigSpec{
					Type: identityv1.DeliveryTypeSelfHostedIRSA,
				},
			},
		},
		{
			name: "non self hosted config",
			obj: &identityv1.AWSWorkloadIdentityConfig{
				Spec: identityv1.AWSWorkloadIdentityConfigSpec{
					Type: identityv1.DeliveryTypeEKSPodIdentity,
				},
				Status: identityv1.AWSWorkloadIdentityConfigStatus{
					ResolvedClusterName: testResolvedClusterName,
				},
			},
		},
		{
			name: "wrong type",
			obj:  &corev1.ServiceAccount{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IndexAWSWorkloadIdentityConfigByResolvedCluster(tt.obj); got != nil {
				t.Fatalf("expected nil index, got %v", got)
			}
		})
	}
}
