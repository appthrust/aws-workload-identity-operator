package inventory

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
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
				LabelOCMClusterName:                             testClusterName,
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
			AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{Name: ocmClusterProfileManagerName}},
			Properties: []clusterinventoryv1alpha1.Property{{
				Name:  PropertyEKSClusterName,
				Value: "prod",
			}, {
				Name:  PropertyAWSRegion,
				Value: "us-east-1",
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

	if resolved.EKSClusterName != "prod" || resolved.AWSRegion != "us-east-1" {
		t.Fatalf("expected properties to be copied: %#v", resolved)
	}
}

func TestResolverIgnoresSlashFormAWSRegionProperty(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	profile := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testClusterName,
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
				Name:  "aws.identity.appthrust.io/aws-region",
				Value: "us-west-2",
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

	if resolved.AWSRegion != "" {
		t.Fatalf("AWSRegion = %q, want empty because slash-form property is not supported", resolved.AWSRegion)
	}
}

func TestResolverNotReadyUntilAccessProviderPresent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	profile := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testClusterName,
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
			Properties: []clusterinventoryv1alpha1.Property{{
				Name:  PropertyEKSClusterName,
				Value: "prod",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile).WithStatusSubresource(profile).Build()

	resolved, err := Resolver{Client: c}.Resolve(context.Background(), testClusterName)
	if err != nil {
		t.Fatalf("unexpected error: %#v", err)
	}

	if resolved.Ready {
		t.Fatalf("expected not-ready resolution while access providers are absent: %#v", resolved)
	}

	if resolved.Reason != identityv1.ReasonInventoryUnavailable {
		t.Fatalf("expected reason %q, got %q (resolution: %#v)", identityv1.ReasonInventoryUnavailable, resolved.Reason, resolved)
	}

	if resolved.ClusterName.Name != testClusterName {
		t.Fatalf("expected cluster name %q, got %q (resolution: %#v)", testClusterName, resolved.ClusterName.Name, resolved)
	}
}

func TestHasAccessProviderRequiresAccessProviders(t *testing.T) {
	// CredentialProviders alone do not satisfy remote rest.Config construction;
	// the remote consumer strips CredentialProviders and reads AccessProviders.
	// Only AccessProviders make the ClusterProfile considered ready.
	cases := []struct {
		name        string
		access      []clusterinventoryv1alpha1.AccessProvider
		credentials []clusterinventoryv1alpha1.CredentialProvider
		want        bool
	}{
		{
			name:        "access providers only",
			access:      []clusterinventoryv1alpha1.AccessProvider{{Name: ocmClusterProfileManagerName}},
			credentials: nil,
			want:        true,
		},
		{
			name:   "credential providers only",
			access: nil,
			credentials: []clusterinventoryv1alpha1.CredentialProvider{{
				Name: "legacy",
			}},
			want: false,
		},
		{
			name:        "both empty",
			access:      nil,
			credentials: nil,
			want:        false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := &clusterinventoryv1alpha1.ClusterProfile{
				Status: clusterinventoryv1alpha1.ClusterProfileStatus{
					AccessProviders:     tc.access,
					CredentialProviders: tc.credentials,
				},
			}

			if got := hasAccessProvider(profile); got != tc.want {
				t.Fatalf("hasAccessProvider() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolverNotReadyWhenOnlyCredentialProviders(t *testing.T) {
	// Regression: a ClusterProfile with only CredentialProviders must surface
	// as not-ready (ReasonInventoryUnavailable). The remote rest.Config
	// consumer strips CredentialProviders and requires AccessProviders, so
	// treating credentials-only profiles as ready is inconsistent.
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	profile := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testClusterName,
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
			CredentialProviders: []clusterinventoryv1alpha1.CredentialProvider{{
				Name: "legacy",
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
		t.Fatalf("unexpected error: %#v", err)
	}

	if resolved.Ready {
		t.Fatalf("expected not-ready resolution when only CredentialProviders are present: %#v", resolved)
	}

	if resolved.Reason != identityv1.ReasonInventoryUnavailable {
		t.Fatalf("expected reason %q, got %q (resolution: %#v)", identityv1.ReasonInventoryUnavailable, resolved.Reason, resolved)
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

func TestResolverFailsClosedWhenMultipleReadyClusterProfilesMatch(t *testing.T) {
	// Two ClusterProfiles in different hub namespaces carry the same OCM
	// cluster-name label and both expose AccessProviders. Silently picking one
	// would route remote-cluster credentials at an arbitrary collision winner,
	// so the resolver must fail closed with ReasonInventoryAmbiguous and list
	// every colliding profile in the Message for operator triage.
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	makeProfile := func(namespace string) *clusterinventoryv1alpha1.ClusterProfile {
		return &clusterinventoryv1alpha1.ClusterProfile{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
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
				Properties:      []clusterinventoryv1alpha1.Property{{Name: PropertyEKSClusterName, Value: namespace}},
			},
		}
	}

	// Insert profiles in a non-sorted order so the test rules out "List
	// returned them already sorted" as the reason ordering looks stable.
	hubC := makeProfile("hub-c")
	hubA := makeProfile("hub-a")
	hubB := makeProfile("hub-b")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hubC, hubA, hubB).WithStatusSubresource(hubC, hubA, hubB).Build()

	resolved, err := Resolver{Client: c}.Resolve(context.Background(), testClusterName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolved.Ready {
		t.Fatalf("expected fail-closed resolution when multiple ready ClusterProfiles match: %#v", resolved)
	}

	if resolved.Reason != identityv1.ReasonInventoryAmbiguous {
		t.Fatalf("expected reason %q, got %q (resolution: %#v)", identityv1.ReasonInventoryAmbiguous, resolved.Reason, resolved)
	}

	for _, want := range []string{"hub-a/" + testClusterName, "hub-b/" + testClusterName, "hub-c/" + testClusterName} {
		if !strings.Contains(resolved.Message, want) {
			t.Fatalf("expected ambiguity message to list %q, got %q", want, resolved.Message)
		}
	}

	// Stable ordering: every colliding profile must appear in (namespace, name)
	// order so reconcile loops do not flap the condition Message between
	// equally-valid orderings. Cover N>2 to catch sort-comparator regressions
	// that two-item tests would miss.
	idxA := strings.Index(resolved.Message, "hub-a/")
	idxB := strings.Index(resolved.Message, "hub-b/")
	idxC := strings.Index(resolved.Message, "hub-c/")

	if idxA < 0 || idxB < 0 || idxC < 0 || idxA >= idxB || idxB >= idxC {
		t.Fatalf("expected stable hub-a < hub-b < hub-c ordering in message, got %q", resolved.Message)
	}
}

func TestResolverPicksSingleReadyClusterProfileWhenNotReadyPeerExists(t *testing.T) {
	// Two ClusterProfiles share the OCM cluster-name label but only one carries
	// AccessProviders. The other is still being provisioned and is not usable,
	// so there is no real ambiguity and the resolver returns the ready profile.
	// This guards against an over-strict fail-closed that would block legitimate
	// transient states (e.g., decommissioned peer that lost AccessProviders).
	scheme := runtime.NewScheme()
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const wantEKSClusterName = "prod"

	ready := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "hub-a",
			Name:      testClusterName,
			Labels: map[string]string{
				LabelOCMClusterName:                             testClusterName,
				clusterinventoryv1alpha1.LabelClusterManagerKey: ocmClusterProfileManagerName,
			},
		},
		Status: clusterinventoryv1alpha1.ClusterProfileStatus{
			AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{Name: ocmClusterProfileManagerName}},
			Properties:      []clusterinventoryv1alpha1.Property{{Name: PropertyEKSClusterName, Value: wantEKSClusterName}},
		},
	}
	pending := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "hub-b",
			Name:      testClusterName,
			Labels: map[string]string{
				LabelOCMClusterName:                             testClusterName,
				clusterinventoryv1alpha1.LabelClusterManagerKey: ocmClusterProfileManagerName,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready, pending).WithStatusSubresource(ready, pending).Build()

	resolved, err := Resolver{Client: c}.Resolve(context.Background(), testClusterName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resolved.Ready {
		t.Fatalf("expected ready resolution when only one peer has AccessProviders: %#v", resolved)
	}

	if resolved.EKSClusterName != wantEKSClusterName {
		t.Fatalf("expected EKSClusterName=%q, got %q (resolution: %#v)", wantEKSClusterName, resolved.EKSClusterName, resolved)
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

func TestResolverIgnoresDirectGetForbiddenWhenOCMResolves(t *testing.T) {
	// The OCM path must not depend on the operator having Get-by-name access
	// on ClusterProfile. Even if direct-name Get is forbidden, when the OCM
	// label-List finds a matching profile the resolution succeeds without
	// touching Get. This locks in the AGENTS.md "ManagedClusterSetBinding-only"
	// RBAC guardrail.
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
		Status: clusterinventoryv1alpha1.ClusterProfileStatus{
			AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{Name: ocmClusterProfileManagerName}},
			Properties:      []clusterinventoryv1alpha1.Property{{Name: PropertyEKSClusterName, Value: "ocm-only"}},
		},
	}
	getCalled := false
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(profile).
		WithStatusSubresource(profile).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*clusterinventoryv1alpha1.ClusterProfile); ok {
					getCalled = true

					return apierrors.NewForbidden(
						clusterinventoryv1alpha1.ClusterProfileSchemeGroupVersionResource.GroupResource(),
						key.Name,
						errors.New("forbidden by RBAC"),
					)
				}

				return inner.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	resolved, err := Resolver{Client: c}.Resolve(context.Background(), testClusterName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resolved.Ready {
		t.Fatalf("expected ready resolution: %#v", resolved)
	}

	if getCalled {
		t.Fatalf("Resolver must not call Get on ClusterProfile when the OCM label-List succeeds")
	}

	if resolved.EKSClusterName != "ocm-only" {
		t.Fatalf("expected EKSClusterName=%q, got %q", "ocm-only", resolved.EKSClusterName)
	}
}
