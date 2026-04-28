package remoteirsa

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

const (
	// LabelOCMClusterName mirrors open-cluster-management.io/api/cluster/v1.ClusterNameLabelKey.
	// Vendoring the OCM API type tree just for one constant is heavy; keep this in sync.
	LabelOCMClusterName = "open-cluster-management.io/cluster-name"

	// OCMClusterProfileManagerName is the spec.clusterManager.name value OCM
	// publishes for ClusterProfile objects it owns.
	OCMClusterProfileManagerName = "open-cluster-management"

	// PropertyAWSRegion is the ClusterProfile property used when a Cluster
	// Inventory publisher exposes the target AWS region through a
	// ClusterProperty CR. Keep this value a valid Kubernetes object name.
	PropertyAWSRegion = "aws.identity.appthrust.io.aws-region"
)

// NewHubResolver returns the default hub API resolver.
func NewHubResolver(reader client.Reader) HubResolver {
	return hubResolver{reader: reader}
}

type hubResolver struct {
	reader client.Reader
}

func (r hubResolver) Resolve(ctx context.Context, opts ResolveOptions) (ResolvedRole, error) { //nolint:funlen,gocritic,cyclop // Public interface keeps value options; resolution is a linear validation pipeline.
	errCtx := errorContext{
		workloadNamespace: opts.WorkloadNamespace,
		serviceAccount:    opts.ServiceAccount,
	}
	if r.reader == nil {
		return ResolvedRole{}, newError(ReasonInvalidOptions, "HubReader is nil", nil, errCtx)
	}

	configRef := types.NamespacedName{Namespace: opts.WorkloadNamespace, Name: identityv1.DefaultName}
	config := &identityv1.AWSWorkloadIdentityConfig{}

	if err := r.reader.Get(ctx, configRef, config); err != nil {
		return ResolvedRole{}, newError(ReasonConfigNotFound, "AWSWorkloadIdentityConfig/default is not available", err, errCtx)
	}

	if !config.Spec.Type.UsesAnnotationBasedIRSA() {
		return ResolvedRole{}, newError(
			ReasonUnsupportedDeliveryType,
			fmt.Sprintf("delivery type %q is not supported by remote IRSA", config.Spec.Type),
			nil,
			errCtx,
		)
	}

	if config.Status.ObservedGeneration < config.Generation {
		return ResolvedRole{}, newError(
			ReasonConfigNotReady,
			fmt.Sprintf("AWSWorkloadIdentityConfig status.observedGeneration %d is behind metadata.generation %d", config.Status.ObservedGeneration, config.Generation),
			nil,
			errCtx,
		)
	}

	configReady := meta.FindStatusCondition(config.Status.Conditions, identityv1.ConditionReady)
	if configReady == nil {
		return ResolvedRole{}, newError(ReasonConfigNotReady, "AWSWorkloadIdentityConfig Ready condition is missing", nil, errCtx)
	}

	if configReady.Status != metav1.ConditionTrue {
		return ResolvedRole{}, newError(
			ReasonConfigNotReady,
			fmt.Sprintf("AWSWorkloadIdentityConfig Ready condition is %s", configReady.Status),
			nil,
			errCtx,
		)
	}

	if configReady.ObservedGeneration < config.Generation {
		return ResolvedRole{}, newError(
			ReasonConfigNotReady,
			fmt.Sprintf("AWSWorkloadIdentityConfig Ready condition observedGeneration %d is behind metadata.generation %d", configReady.ObservedGeneration, config.Generation),
			nil,
			errCtx,
		)
	}

	role, err := r.resolveRole(ctx, &opts)
	if err != nil {
		return ResolvedRole{}, err
	}

	roleRef := types.NamespacedName{Namespace: role.Namespace, Name: role.Name}
	errCtx.roleRef = roleRef

	roleSA := types.NamespacedName{
		Namespace: role.Spec.ServiceAccount.Namespace,
		Name:      role.Spec.ServiceAccount.Name,
	}
	if roleSA != opts.ServiceAccount {
		return ResolvedRole{}, newError(
			ReasonRoleServiceAccountMismatch,
			fmt.Sprintf("AWSServiceAccountRole serviceAccount %s does not match requested ServiceAccount %s", roleSA, opts.ServiceAccount),
			nil,
			errCtx,
		)
	}

	if role.Status.RoleARN == "" {
		return ResolvedRole{}, newError(ReasonRoleARNNotReady, "AWSServiceAccountRole status.roleARN is empty", nil, errCtx)
	}

	if role.Status.ObservedGeneration < role.Generation {
		return ResolvedRole{}, newError(
			ReasonRoleARNNotReady,
			fmt.Sprintf("AWSServiceAccountRole status.observedGeneration %d is behind metadata.generation %d", role.Status.ObservedGeneration, role.Generation),
			nil,
			errCtx,
		)
	}

	readyCond := meta.FindStatusCondition(role.Status.Conditions, identityv1.ConditionReady)
	if readyCond == nil {
		return ResolvedRole{}, newError(ReasonRoleARNNotReady, "AWSServiceAccountRole Ready condition is missing", nil, errCtx)
	}

	if readyCond.Status != metav1.ConditionTrue {
		return ResolvedRole{}, newError(
			ReasonRoleARNNotReady,
			fmt.Sprintf("AWSServiceAccountRole Ready condition is %s", readyCond.Status),
			nil,
			errCtx,
		)
	}

	if readyCond.ObservedGeneration < role.Generation {
		return ResolvedRole{}, newError(
			ReasonRoleARNNotReady,
			fmt.Sprintf("AWSServiceAccountRole Ready condition observedGeneration %d is behind metadata.generation %d", readyCond.ObservedGeneration, role.Generation),
			nil,
			errCtx,
		)
	}

	region := config.Spec.Region
	if opts.RegionOverride != "" {
		region = opts.RegionOverride
	}

	if region == "" {
		return ResolvedRole{}, newError(ReasonRegionNotReady, "AWS region is empty", nil, errCtx)
	}

	return ResolvedRole{
		WorkloadNamespace: opts.WorkloadNamespace,
		ConfigRef:         configRef,
		RoleRef:           roleRef,
		ServiceAccount:    opts.ServiceAccount,
		RoleARN:           role.Status.RoleARN,
		Region:            region,
		DeliveryType:      string(config.Spec.Type),
	}, nil
}

func (r hubResolver) resolveRole(ctx context.Context, opts *ResolveOptions) (*identityv1.AWSServiceAccountRole, error) {
	errCtx := errorContext{
		workloadNamespace: opts.WorkloadNamespace,
		serviceAccount:    opts.ServiceAccount,
	}

	if opts.AWSServiceAccountRoleName != "" {
		roleRef := types.NamespacedName{Namespace: opts.WorkloadNamespace, Name: opts.AWSServiceAccountRoleName}

		role := &identityv1.AWSServiceAccountRole{}
		if err := r.reader.Get(ctx, roleRef, role); err != nil {
			errCtx.roleRef = roleRef

			return nil, newError(ReasonRoleNotFound, "AWSServiceAccountRole is not available", err, errCtx)
		}

		return role, nil
	}

	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := r.reader.List(ctx, roles, client.InNamespace(opts.WorkloadNamespace)); err != nil {
		return nil, newError(ReasonRoleNotFound, "list AWSServiceAccountRole objects", err, errCtx)
	}

	matches := make([]*identityv1.AWSServiceAccountRole, 0, 1)

	for i := range roles.Items {
		role := &roles.Items[i]
		if !role.DeletionTimestamp.IsZero() {
			continue
		}

		if role.Spec.ServiceAccount.Namespace == opts.ServiceAccount.Namespace && role.Spec.ServiceAccount.Name == opts.ServiceAccount.Name {
			matches = append(matches, role)
		}
	}

	slices.SortFunc(matches, func(a, b *identityv1.AWSServiceAccountRole) int {
		return cmp.Compare(a.Name, b.Name)
	})

	switch len(matches) {
	case 0:
		return nil, newError(ReasonRoleNotFound, "no AWSServiceAccountRole matches ServiceAccount", nil, errCtx)
	case 1:
		return matches[0], nil
	default:
		return nil, newError(ReasonMultipleRoles, "multiple AWSServiceAccountRole objects match ServiceAccount; set AWSServiceAccountRoleName", nil, errCtx)
	}
}

// NewRemoteConfigResolver returns the default ClusterProfile-backed remote
// rest.Config resolver.
func NewRemoteConfigResolver(reader client.Reader) RemoteConfigResolver {
	return NewRemoteConfigResolverWithOptions(reader, RemoteConfigResolverOptions{})
}

// RemoteConfigResolverOptions configures the default ClusterProfile-backed
// remote rest.Config resolver.
type RemoteConfigResolverOptions struct {
	// ClusterProfileNamespaces limits the OCM cluster-name label List to these
	// namespaces. Empty preserves the historical all-namespace lookup. Use "*"
	// to explicitly request all namespaces.
	ClusterProfileNamespaces []string
}

// NewRemoteConfigResolverWithOptions returns the default ClusterProfile-backed
// remote rest.Config resolver with explicit lookup options.
func NewRemoteConfigResolverWithOptions(reader client.Reader, opts RemoteConfigResolverOptions) RemoteConfigResolver {
	return remoteConfigResolver{
		reader:                   reader,
		clusterProfileNamespaces: normalizeResolverClusterProfileNamespaces(opts.ClusterProfileNamespaces),
	}
}

type remoteConfigResolver struct {
	reader                   client.Reader
	clusterProfileNamespaces []string
}

func (r remoteConfigResolver) ResolveRemoteConfig(ctx context.Context, workloadNamespace string, accessConfig *access.Config) (*rest.Config, ResolvedClusterProfile, error) {
	errCtx := errorContext{workloadNamespace: workloadNamespace}
	if r.reader == nil {
		return nil, ResolvedClusterProfile{}, newError(ReasonInvalidOptions, "HubReader is nil", nil, errCtx)
	}

	if accessConfig == nil {
		return nil, ResolvedClusterProfile{}, newError(ReasonMissingInventoryAccess, "ClusterProfile access config is nil", nil, errCtx)
	}

	profile, err := r.resolveClusterProfile(ctx, workloadNamespace)
	if err != nil {
		return nil, ResolvedClusterProfile{}, err
	}

	resolved := ResolvedClusterProfile{
		Ref: types.NamespacedName{
			Namespace: profile.Namespace,
			Name:      profile.Name,
		},
		ProviderName: selectedAccessProviderName(accessConfig, profile),
		AWSRegion:    clusterProfileProperty(profile, PropertyAWSRegion),
	}
	errCtx.clusterProfileRef = resolved.Ref
	errCtx.providerName = resolved.ProviderName

	if len(profile.Status.AccessProviders) == 0 {
		return nil, resolved, newError(ReasonMissingInventoryAccess, "ClusterProfile has no status.accessProviders", nil, errCtx)
	}

	if resolved.ProviderName == "" {
		return nil, resolved, newError(ReasonMissingInventoryAccess, "ClusterProfile has no status.accessProviders entry matching the access config", nil, errCtx)
	}

	cfg, err := CloneAccessConfig(accessConfig).BuildConfigFromCP(accessProviderOnlyProfile(profile))
	if err != nil {
		return nil, resolved, newError(ReasonMissingInventoryAccess, "build remote Kubernetes rest.Config from ClusterProfile status.accessProviders", err, errCtx)
	}

	return cfg, resolved, nil
}

// resolveClusterProfile locates the ClusterProfile for workloadNamespace by
// listing ClusterProfile objects carrying the OCM cluster-name label equal to
// the workload namespace and the cluster-manager label equal to
// open-cluster-management. Exactly one matching profile must carry
// Status.AccessProviders; the resolver fails closed when more than one does so
// remote credentials are never routed at an arbitrary collision winner.
func (r remoteConfigResolver) resolveClusterProfile(ctx context.Context, workloadNamespace string) (*clusterinventoryv1alpha1.ClusterProfile, error) {
	profiles, err := r.listClusterProfiles(ctx, workloadNamespace)
	if err != nil {
		return nil, newError(ReasonMissingInventoryAccess, "list OCM ClusterProfiles by cluster-name label", err, errorContext{
			workloadNamespace: workloadNamespace,
		})
	}

	if len(profiles.Items) == 0 {
		return nil, newError(ReasonMissingInventoryAccess, "ClusterProfile was not found by OCM cluster-name label", nil, errorContext{
			workloadNamespace: workloadNamespace,
		})
	}

	ready := PartitionClusterProfilesByAccess(profiles.Items)

	switch len(ready) {
	case 0:
		first := types.NamespacedName{Namespace: profiles.Items[0].Namespace, Name: profiles.Items[0].Name}

		return nil, newError(ReasonMissingInventoryAccess, "OCM ClusterProfiles matched but none has status.accessProviders", nil, errorContext{
			workloadNamespace: workloadNamespace,
			clusterProfileRef: first,
		})
	case 1:
		return ready[0], nil
	default:
		matches := make([]string, 0, len(ready))
		for _, p := range ready {
			matches = append(matches, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}.String())
		}

		return nil, newError(
			ReasonMultipleClusterProfiles,
			fmt.Sprintf("multiple OCM ClusterProfiles carry AccessProviders for cluster-name label %q: %s", workloadNamespace, strings.Join(matches, ", ")),
			nil,
			errorContext{
				workloadNamespace: workloadNamespace,
				clusterProfileRef: types.NamespacedName{Namespace: ready[0].Namespace, Name: ready[0].Name},
			},
		)
	}
}

func (r remoteConfigResolver) listClusterProfiles(ctx context.Context, workloadNamespace string) (*clusterinventoryv1alpha1.ClusterProfileList, error) {
	labels := client.MatchingLabels{
		LabelOCMClusterName:                             workloadNamespace,
		clusterinventoryv1alpha1.LabelClusterManagerKey: OCMClusterProfileManagerName,
	}
	if len(r.clusterProfileNamespaces) == 0 {
		profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
		if err := r.reader.List(ctx, profiles, labels); err != nil {
			return nil, err
		}

		return profiles, nil
	}

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	for _, namespace := range r.clusterProfileNamespaces {
		scoped := &clusterinventoryv1alpha1.ClusterProfileList{}
		if err := r.reader.List(ctx, scoped, client.InNamespace(namespace), labels); err != nil {
			return nil, err
		}

		profiles.Items = append(profiles.Items, scoped.Items...)
	}

	return profiles, nil
}

func normalizeResolverClusterProfileNamespaces(namespaces []string) []string {
	seen := make(map[string]struct{}, len(namespaces))
	normalized := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			continue
		}
		if namespace == "*" {
			return nil
		}
		if _, ok := seen[namespace]; ok {
			continue
		}

		seen[namespace] = struct{}{}
		normalized = append(normalized, namespace)
	}

	slices.Sort(normalized)

	return normalized
}

// PartitionClusterProfilesByAccess sorts items in place by (namespace, name)
// so candidate ordering is stable across reconciles, and returns pointers to
// the subset whose Status.AccessProviders is non-empty. The OCM cluster-name
// label can resolve to multiple ClusterProfile rows; remote rest.Config is
// built solely from AccessProviders, so callers gate readiness on this subset
// and map the (sorted, ready) split to package-specific error sentinels.
func PartitionClusterProfilesByAccess(items []clusterinventoryv1alpha1.ClusterProfile) []*clusterinventoryv1alpha1.ClusterProfile {
	slices.SortFunc(items, func(a, b clusterinventoryv1alpha1.ClusterProfile) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	ready := make([]*clusterinventoryv1alpha1.ClusterProfile, 0, len(items))
	for i := range items {
		if len(items[i].Status.AccessProviders) > 0 {
			ready = append(ready, &items[i])
		}
	}

	return ready
}

func selectedAccessProviderName(accessConfig *access.Config, profile *clusterinventoryv1alpha1.ClusterProfile) string {
	if accessConfig == nil || profile == nil {
		return ""
	}

	accessProviders := make(map[string]struct{}, len(profile.Status.AccessProviders))
	for i := range profile.Status.AccessProviders {
		provider := &profile.Status.AccessProviders[i]
		accessProviders[provider.Name] = struct{}{}
	}

	for _, provider := range accessConfig.Providers {
		if _, ok := accessProviders[provider.Name]; ok {
			return provider.Name
		}
	}

	return ""
}

func clusterProfileProperty(profile *clusterinventoryv1alpha1.ClusterProfile, name string) string {
	if profile == nil {
		return ""
	}

	for _, property := range profile.Status.Properties {
		if property.Name == name {
			return property.Value
		}
	}

	return ""
}

func accessProviderOnlyProfile(profile *clusterinventoryv1alpha1.ClusterProfile) *clusterinventoryv1alpha1.ClusterProfile {
	out := profile.DeepCopy()
	out.Status.CredentialProviders = nil

	return out
}

// CloneAccessConfig returns a copy of accessConfig that is safe to mutate
// independently. ExecConfig pointers are deep-copied so callers can pass the
// result to BuildConfigFromCP without leaking per-call state back into the
// shared access config (the access library mutates the slice in place).
func CloneAccessConfig(in *access.Config) *access.Config {
	providers := make([]access.Provider, len(in.Providers))
	for i := range in.Providers {
		providers[i] = in.Providers[i]
		if in.Providers[i].ExecConfig != nil {
			providers[i].ExecConfig = in.Providers[i].ExecConfig.DeepCopy()
		}
	}

	return access.New(providers)
}
