package inventory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

// Property constants are ClusterProfile property names used by the resolver.
const (
	PropertyEKSClusterName    = "aws.identity.appthrust.io/eks-cluster-name"
	PropertyEKSClusterARN     = "aws.identity.appthrust.io/eks-cluster-arn"
	PropertyAWSAccountID      = "aws.identity.appthrust.io/aws-account-id"
	PropertyAWSRegion         = remoteirsa.PropertyAWSRegion
	PropertyAWSOrganizationID = "aws.identity.appthrust.io/aws-organization-id"
	PropertyEKSAutoMode       = "aws.identity.appthrust.io/eks-auto-mode"
)

const (
	// LabelOCMClusterName mirrors open-cluster-management.io/api/cluster/v1.ClusterNameLabelKey.
	// Re-exported from pkg/remoteirsa to avoid drift between inventory and remote resolvers.
	LabelOCMClusterName            = remoteirsa.LabelOCMClusterName
	ocmClusterProfileManagerName   = remoteirsa.OCMClusterProfileManagerName
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

// ambiguousOCMClusterProfileError is returned when multiple ClusterProfiles
// match the workload namespace's OCM cluster-name label and more than one
// carries AccessProviders. Picking one silently could route IAM/IRSA delivery
// at the wrong remote cluster, so the resolver fails closed and surfaces the
// collision so an operator can resolve the misconfiguration.
type ambiguousOCMClusterProfileError struct {
	Namespace string
	Matches   []types.NamespacedName
}

func (e *ambiguousOCMClusterProfileError) Error() string {
	names := make([]string, 0, len(e.Matches))
	for _, m := range e.Matches {
		names = append(names, m.String())
	}

	return fmt.Sprintf(
		"multiple OCM ClusterProfiles match cluster-name label %q with AccessProviders: %s",
		e.Namespace,
		strings.Join(names, ", "),
	)
}

// errOCMClusterProfileAmbiguousSentinel is the errors.Is target for
// ambiguousOCMClusterProfileError. It carries no detail; callers should
// errors.As the typed value when they need the collision list.
var errOCMClusterProfileAmbiguousSentinel = errors.New("multiple OCM ClusterProfiles match the workload namespace")

// Is matches the sentinel above so call sites can branch on
// errors.Is(err, errOCMClusterProfileAmbiguousSentinel) without taking a
// dependency on the typed error shape.
func (e *ambiguousOCMClusterProfileError) Is(target error) bool {
	return target == errOCMClusterProfileAmbiguousSentinel
}

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
	AWSRegion         string
	AWSOrganizationID string
	EKSAutoMode       bool
}

// Resolve loads the namespace's ClusterProfile and extracts AWS identity properties.
//
// Resolution is label-based: the resolver lists ClusterProfile objects carrying
// the OCM `open-cluster-management.io/cluster-name` label equal to the workload
// namespace and the `x-k8s.io/cluster-manager` label equal to
// `open-cluster-management`. Three sentinels are surfaced as Resolution Reasons:
// errOCMClusterProfileNotFound becomes ReasonClusterProfileNotFound;
// errOCMClusterProfileNotReady becomes ReasonInventoryUnavailable (matching
// profiles exist but have not been provisioned with AccessProviders yet); and
// ambiguousOCMClusterProfileError becomes ReasonInventoryAmbiguous (more than
// one ready ClusterProfile matches the workload namespace, so the resolver
// fails closed instead of routing remote credentials at an arbitrary winner).
func (r Resolver) Resolve(ctx context.Context, namespace string) (Resolution, error) {
	if r.Client == nil {
		return Resolution{}, errors.New("inventory resolver client is nil")
	}

	key := types.NamespacedName{Namespace: namespace, Name: namespace}

	profile, err := r.findOCMClusterProfile(ctx, namespace)
	switch {
	case err == nil:
		// OCM-labeled profile found; fall through to access-provider check.
	case errors.Is(err, errOCMClusterProfileNotReady):
		return Resolution{
			ClusterName: key,
			Ready:       false,
			Reason:      identityv1.ReasonInventoryUnavailable,
			Message:     fmt.Sprintf("OCM ClusterProfile for workload namespace %s exists but has not been provisioned with access providers yet", namespace),
		}, nil
	case errors.Is(err, errOCMClusterProfileNotFound):
		return Resolution{
			ClusterName: key,
			Ready:       false,
			Reason:      identityv1.ReasonClusterProfileNotFound,
			Message:     fmt.Sprintf("ClusterProfile for workload namespace %s was not found", namespace),
		}, nil
	case errors.Is(err, errOCMClusterProfileAmbiguousSentinel):
		return Resolution{
			ClusterName: key,
			Ready:       false,
			Reason:      identityv1.ReasonInventoryAmbiguous,
			Message:     err.Error(),
		}, nil
	default:
		return Resolution{}, err
	}

	if !hasAccessProvider(profile) {
		return Resolution{
			ClusterName: logicalClusterName(profile),
			Ready:       false,
			Reason:      identityv1.ReasonInventoryUnavailable,
			Message:     fmt.Sprintf("ClusterProfile for workload namespace %s exists but has not been provisioned with access providers yet", namespace),
		}, nil
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

	ready := remoteirsa.PartitionClusterProfilesByAccess(profiles.Items)

	switch len(ready) {
	case 0:
		// Matched profiles exist but none carry AccessProviders/CredentialProviders.
		// This is distinct from "not found": the cluster is registered with OCM but
		// the managed-serviceaccount controller has not finished provisioning
		// credentials yet. Returning a more specific sentinel lets the caller
		// surface a transient-state Reason rather than the misleading
		// ClusterProfileNotFound.
		return nil, errOCMClusterProfileNotReady
	case 1:
		return ready[0], nil
	default:
		// More than one ready ClusterProfile resolves to the same workload
		// namespace via the OCM cluster-name label. Picking one would route
		// remote-cluster credentials at an arbitrary collision winner, so fail
		// closed and surface every collision so an operator can correct the
		// hub-side labelling or ManagedClusterSetBinding scope.
		matches := make([]types.NamespacedName, 0, len(ready))
		for _, p := range ready {
			matches = append(matches, types.NamespacedName{Namespace: p.Namespace, Name: p.Name})
		}

		return nil, &ambiguousOCMClusterProfileError{Namespace: namespace, Matches: matches}
	}
}

func resolutionForProfile(profile *clusterinventoryv1alpha1.ClusterProfile) Resolution {
	properties := propertyMap(profile.Status.Properties)
	clusterName := logicalClusterName(profile)

	return Resolution{
		ClusterName:       clusterName,
		Ready:             true,
		Reason:            identityv1.ReasonResolved,
		Message:           defaultClusterProfileReadyText,
		EKSClusterName:    properties[PropertyEKSClusterName],
		EKSClusterARN:     properties[PropertyEKSClusterARN],
		AWSAccountID:      properties[PropertyAWSAccountID],
		AWSRegion:         properties[PropertyAWSRegion],
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
// namespace whose AWSWorkloadIdentityConfig depends on it. OCM-managed
// ClusterProfiles carry the workload cluster name in
// open-cluster-management.io/cluster-name; the value of that label is the
// workload namespace.
func WorkloadNamespaceForClusterProfile(profile *clusterinventoryv1alpha1.ClusterProfile) string {
	if profile.Labels != nil && profile.Labels[LabelOCMClusterName] != "" {
		return profile.Labels[LabelOCMClusterName]
	}

	return ""
}

func hasAccessProvider(profile *clusterinventoryv1alpha1.ClusterProfile) bool {
	// Remote rest.Config is built solely from AccessProviders (see pkg/remoteirsa/resolve.go); CredentialProviders alone cannot satisfy downstream consumers.
	return len(profile.Status.AccessProviders) > 0
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
