package remoteirsa

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func (r hubResolver) Resolve(ctx context.Context, opts ResolveOptions) (ResolvedRole, error) { //nolint:funlen,gocritic // Public interface keeps value options; resolution is a linear validation pipeline.
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
	return remoteConfigResolver{reader: reader}
}

type remoteConfigResolver struct {
	reader client.Reader
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

func (r remoteConfigResolver) resolveClusterProfile(ctx context.Context, workloadNamespace string) (*clusterinventoryv1alpha1.ClusterProfile, error) {
	directRef := types.NamespacedName{Namespace: workloadNamespace, Name: workloadNamespace}

	profile := &clusterinventoryv1alpha1.ClusterProfile{}
	if err := r.reader.Get(ctx, directRef, profile); err == nil {
		return profile, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, newError(ReasonMissingInventoryAccess, "get direct ClusterProfile", err, errorContext{
			workloadNamespace: workloadNamespace,
			clusterProfileRef: directRef,
		})
	}

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := r.reader.List(ctx, profiles, client.MatchingLabels{
		LabelOCMClusterName:                             workloadNamespace,
		clusterinventoryv1alpha1.LabelClusterManagerKey: OCMClusterProfileManagerName,
	}); err != nil {
		return nil, newError(ReasonMissingInventoryAccess, "list OCM ClusterProfiles by cluster-name label", err, errorContext{
			workloadNamespace: workloadNamespace,
		})
	}

	if len(profiles.Items) == 0 {
		return nil, newError(ReasonMissingInventoryAccess, "ClusterProfile was not found by direct namespace/name or OCM cluster-name label", nil, errorContext{
			workloadNamespace: workloadNamespace,
			clusterProfileRef: directRef,
		})
	}

	slices.SortFunc(profiles.Items, func(a, b clusterinventoryv1alpha1.ClusterProfile) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	for i := range profiles.Items {
		if len(profiles.Items[i].Status.AccessProviders) > 0 {
			return &profiles.Items[i], nil
		}
	}

	first := types.NamespacedName{Namespace: profiles.Items[0].Namespace, Name: profiles.Items[0].Name}

	return nil, newError(ReasonMissingInventoryAccess, "OCM ClusterProfiles matched but none has status.accessProviders", nil, errorContext{
		workloadNamespace: workloadNamespace,
		clusterProfileRef: first,
	})
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
