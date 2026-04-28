package inventory

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testClusterName = "wlc-a"

func TestResolverCopiesClusterProfileProperties(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	profile := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testClusterName,
			Name:      testClusterName,
			Labels: map[string]string{
				clusterinventoryv1alpha1.LabelClusterManagerKey: ocmClusterProfileManagerName,
			},
		},
		Spec: clusterinventoryv1alpha1.ClusterProfileSpec{
			ClusterManager: clusterinventoryv1alpha1.ClusterManager{Name: ocmClusterProfileManagerName},
		},
		Status: clusterinventoryv1alpha1.ClusterProfileStatus{
			Conditions: []metav1.Condition{{
				Type:   clusterinventoryv1alpha1.ClusterConditionControlPlaneHealthy,
				Status: metav1.ConditionTrue,
				Reason: "Healthy",
			}},
			Properties: []clusterinventoryv1alpha1.Property{{
				Name:  PropertyEKSClusterName,
				Value: "prod",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile).WithStatusSubresource(profile).Build()

	resolved, err := Resolver{Client: c}.Resolve(context.Background(), testClusterName)
	if err != nil {
		t.Fatal(err)
	}

	if !resolved.Ready {
		t.Fatalf("expected ready resolution: %#v", resolved)
	}

	if resolved.ClusterName.String() != testClusterName+"/"+testClusterName {
		t.Fatalf("unexpected cluster name: %s", resolved.ClusterName.String())
	}

	if resolved.EKSClusterName != "prod" {
		t.Fatalf("expected properties to be copied")
	}

	if ref := resolved.ObjectReference(); ref == nil || ref.Kind != "ClusterProfile" || ref.Name != testClusterName || ref.Namespace != testClusterName {
		t.Fatalf("unexpected ref: %#v", ref)
	}
}

func TestResolverFindsOCMClusterProfileByClusterNameLabel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	profile := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "awio-system",
			Name:      testClusterName,
			Labels: map[string]string{
				LabelOCMClusterName:                             testClusterName,
				clusterinventoryv1alpha1.LabelClusterManagerKey: ocmClusterProfileManagerName,
			},
		},
		Spec: clusterinventoryv1alpha1.ClusterProfileSpec{
			ClusterManager: clusterinventoryv1alpha1.ClusterManager{Name: ocmClusterProfileManagerName},
		},
		Status: clusterinventoryv1alpha1.ClusterProfileStatus{
			AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{Name: ocmClusterProfileManagerName}},
			Properties: []clusterinventoryv1alpha1.Property{{
				Name:  PropertyEKSClusterName,
				Value: "prod",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile).WithStatusSubresource(profile).Build()

	resolved, err := Resolver{Client: c}.Resolve(context.Background(), testClusterName)
	if err != nil {
		t.Fatal(err)
	}

	if !resolved.Ready {
		t.Fatalf("expected ready resolution: %#v", resolved)
	}

	if resolved.ClusterName.String() != testClusterName+"/"+testClusterName {
		t.Fatalf("unexpected logical cluster name: %s", resolved.ClusterName.String())
	}

	if resolved.EKSClusterName != "prod" {
		t.Fatalf("expected properties to be copied")
	}

	if ref := resolved.ObjectReference(); ref == nil || ref.Kind != "ClusterProfile" || ref.Name != testClusterName || ref.Namespace != "awio-system" {
		t.Fatalf("unexpected ref: %#v", ref)
	}
}

func TestResolverReturnsFalseForMissingClusterProfile(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	resolved, err := Resolver{Client: c}.Resolve(context.Background(), testClusterName)
	if err != nil {
		t.Fatal(err)
	}

	if resolved.Ready || resolved.Reason != "ClusterProfileNotFound" {
		t.Fatalf("expected missing ClusterProfile failure: %#v", resolved)
	}
}

func TestResolutionRequireEKS(t *testing.T) {
	ready := Resolution{Ready: true, EKSClusterName: "prod", EKSClusterARN: "arn", AWSAccountID: "123456789012"}
	if err := ready.RequireEKS(); err != nil {
		t.Fatal(err)
	}

	missing := Resolution{Ready: true}
	if err := missing.RequireEKS(); err == nil {
		t.Fatalf("expected missing EKS fields to fail")
	}
}
