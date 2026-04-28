package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

// Property constants are ClusterProfile property names used by the resolver.
const (
	PropertyEKSClusterName    = "aws.identity.appthrust.io/eks-cluster-name"
	PropertyEKSClusterARN     = "aws.identity.appthrust.io/eks-cluster-arn"
	PropertyAWSAccountID      = "aws.identity.appthrust.io/aws-account-id"
	PropertyAWSOrganizationID = "aws.identity.appthrust.io/aws-organization-id"
	PropertyEKSAutoMode       = "aws.identity.appthrust.io/eks-auto-mode"
)

const (
	// LabelOCMClusterName mirrors open-cluster-management.io/api/cluster/v1.ClusterNameLabelKey.
	// Vendoring the OCM API type tree just for one constant is heavy; keep this in sync.
	LabelOCMClusterName            = "open-cluster-management.io/cluster-name"
	ocmClusterProfileManagerName   = "open-cluster-management"
	defaultClusterProfileReadyText = "ClusterProfile resolved"
)

// errOCMClusterProfileNotFound is the sentinel returned by findOCMClusterProfile
// when the label-filtered List finds no matching ClusterProfile. Callers must
// use errors.Is to detect this and fall back to the not-found Resolution.
var errOCMClusterProfileNotFound = errors.New("no OCM ClusterProfile found for namespace")

// errOCMClusterProfileNotReady is returned when matching ClusterProfiles exist
// but none of them carry AccessProviders/CredentialProviders yet. The cluster
// is registered with OCM but its managed-serviceaccount controller has not
// finished provisioning credentials. Callers surface this as a transient
// "not ready" state distinct from "not found".
var errOCMClusterProfileNotReady = errors.New("OCM ClusterProfile has no access providers yet")

// Resolver resolves ClusterProfile inventory for a workload namespace.
type Resolver struct {
	Client client.Reader
}

// Resolution is the resolved ClusterProfile identity and readiness state. The
// AWS identity fields are projections from `ClusterProfile.status.properties`
// resolved at construction time; do not mutate.
type Resolution struct {
	ClusterName       types.NamespacedName
	Ready             bool
	Reason            string
	Message           string
	EKSClusterName    string
	EKSClusterARN     string
	AWSAccountID      string
	AWSOrganizationID string
	EKSAutoMode       bool

	// profile is retained only to expose ObjectReference. Callers must not
	// reach into status fields directly: that path is what the typed AWS
	// identity fields above replace.
	profile *clusterinventoryv1alpha1.ClusterProfile
}

// Resolve loads the namespace's ClusterProfile and extracts AWS identity properties.
func (r Resolver) Resolve(ctx context.Context, namespace string) (Resolution, error) {
	if r.Client == nil {
		return Resolution{}, errors.New("inventory resolver client is nil")
	}

	key := types.NamespacedName{Namespace: namespace, Name: namespace}
	profile := &clusterinventoryv1alpha1.ClusterProfile{}

	if err := r.Client.Get(ctx, key, profile); err != nil {
		if !apierrors.IsNotFound(err) {
			return Resolution{}, fmt.Errorf("get ClusterProfile %s: %w", key, err)
		}

		fallback, err := r.findOCMClusterProfile(ctx, namespace)
		// Exhaustive switch over the OCM-fallback sentinels so a future
		// sentinel is not silently mapped to the generic error path.
		switch {
		case errors.Is(err, errOCMClusterProfileNotFound):
			return Resolution{
				ClusterName: key,
				Ready:       false,
				Reason:      identityv1.ReasonClusterProfileNotFound,
				Message:     fmt.Sprintf("ClusterProfile for workload namespace %s was not found", namespace),
			}, nil
		case errors.Is(err, errOCMClusterProfileNotReady):
			return Resolution{
				ClusterName: key,
				Ready:       false,
				Reason:      identityv1.ReasonInventoryUnavailable,
				Message:     fmt.Sprintf("OCM ClusterProfile for workload namespace %s exists but has not been provisioned with access providers yet", namespace),
			}, nil
		case err != nil:
			return Resolution{}, err
		}

		profile = fallback
	}

	return resolutionForProfile(profile), nil
}

func (r Resolver) findOCMClusterProfile(ctx context.Context, namespace string) (*clusterinventoryv1alpha1.ClusterProfile, error) {
	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := r.Client.List(ctx, profiles, client.MatchingLabels{
		LabelOCMClusterName:                             namespace,
		clusterinventoryv1alpha1.LabelClusterManagerKey: ocmClusterProfileManagerName,
	}); err != nil {
		return nil, fmt.Errorf("list OCM ClusterProfiles for workload namespace %q: %w", namespace, err)
	}

	if len(profiles.Items) == 0 {
		return nil, errOCMClusterProfileNotFound
	}

	// Alphabetical sort gives deterministic resolution when multiple OCM
	// ClusterProfiles match the same cluster-name label across hub namespaces.
	slices.SortFunc(profiles.Items, func(a, b clusterinventoryv1alpha1.ClusterProfile) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	for i := range profiles.Items {
		if hasAccessProvider(&profiles.Items[i]) {
			return &profiles.Items[i], nil
		}
	}

	// Matched profiles exist but none carry AccessProviders/CredentialProviders.
	// This is distinct from "not found": the cluster is registered with OCM but
	// the managed-serviceaccount controller has not finished provisioning
	// credentials yet. Returning a more specific sentinel lets the caller
	// surface a transient-state Reason rather than the misleading
	// ClusterProfileNotFound.
	return nil, errOCMClusterProfileNotReady
}

func resolutionForProfile(profile *clusterinventoryv1alpha1.ClusterProfile) Resolution {
	properties := propertyMap(profile.Status.Properties)
	clusterName := logicalClusterName(profile)

	return Resolution{
		profile:           profile,
		ClusterName:       clusterName,
		Ready:             true,
		Reason:            identityv1.ReasonResolved,
		Message:           defaultClusterProfileReadyText,
		EKSClusterName:    properties[PropertyEKSClusterName],
		EKSClusterARN:     properties[PropertyEKSClusterARN],
		AWSAccountID:      properties[PropertyAWSAccountID],
		AWSOrganizationID: properties[PropertyAWSOrganizationID],
		EKSAutoMode:       properties[PropertyEKSAutoMode] == "true",
	}
}

// logicalClusterName returns the multicluster-runtime cluster identifier for
// the profile. The operator models each workload cluster as a hub-side
// namespace whose name equals the cluster name, so the identifier is encoded
// as a NamespacedName with both Namespace and Name set to the same string.
// The OCM `cluster-name` label takes precedence over the ClusterProfile
// object's own Name when present, since OCM-managed profiles live in
// `awio-system` (or another hub namespace) but identify themselves by the
// downstream cluster name.
func logicalClusterName(profile *clusterinventoryv1alpha1.ClusterProfile) types.NamespacedName {
	name := profile.Name
	if profile.Labels != nil && profile.Labels[LabelOCMClusterName] != "" {
		name = profile.Labels[LabelOCMClusterName]
	}

	return types.NamespacedName{Namespace: name, Name: name}
}

// WorkloadNamespaceForClusterProfile maps a ClusterProfile event back to the
// namespace whose AWSWorkloadIdentityConfig depends on it. Direct profiles use
// the workload namespace/name convention; OCM fallback profiles carry the
// workload cluster name in open-cluster-management.io/cluster-name.
func WorkloadNamespaceForClusterProfile(profile *clusterinventoryv1alpha1.ClusterProfile) string {
	if profile.Labels != nil && profile.Labels[LabelOCMClusterName] != "" {
		return profile.Labels[LabelOCMClusterName]
	}

	if profile.Namespace == profile.Name {
		return profile.Name
	}

	return ""
}

func hasAccessProvider(profile *clusterinventoryv1alpha1.ClusterProfile) bool {
	return len(profile.Status.AccessProviders) > 0 || len(profile.Status.CredentialProviders) > 0
}

// ObjectReference returns a Kubernetes object reference for the resolved profile.
func (r *Resolution) ObjectReference() *corev1.ObjectReference {
	if r.profile == nil {
		return nil
	}

	return &corev1.ObjectReference{
		APIVersion: clusterinventoryv1alpha1.GroupVersion.String(),
		Kind:       "ClusterProfile",
		Namespace:  r.profile.Namespace,
		Name:       r.profile.Name,
		UID:        r.profile.UID,
	}
}

// RequireEKS validates that the resolution contains EKS identity properties.
func (r *Resolution) RequireEKS() error {
	switch {
	case !r.Ready:
		return fmt.Errorf("%s: %s", r.Reason, r.Message)
	case r.EKSClusterName == "":
		return errors.New("missing EKS cluster name property")
	case r.EKSClusterARN == "":
		return errors.New("missing EKS cluster ARN property")
	case r.AWSAccountID == "":
		return errors.New("missing AWS account ID property")
	default:
		return nil
	}
}

func propertyMap(properties []clusterinventoryv1alpha1.Property) map[string]string {
	out := make(map[string]string, len(properties))
	for _, property := range properties {
		out[property.Name] = property.Value
	}

	return out
}
