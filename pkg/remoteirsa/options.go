package remoteirsa

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// STSAudience is the Kubernetes TokenRequest audience used for AWS STS web
	// identity exchange.
	STSAudience = "sts.amazonaws.com"

	defaultTokenExpiration = 15 * time.Minute
	minSTSSessionDuration  = 15 * time.Minute
	maxSTSSessionDuration  = 12 * time.Hour
)

// Options configures Provider.
type Options struct {
	// Required unless HubResolver is provided. Keep this to the read-only
	// controller-runtime surface used for hub-side API objects.
	HubReader client.Reader

	// Optional. Reserved for default inventory resolvers that need core
	// Kubernetes reads for provider credentials.
	HubKubeClient kubernetes.Interface

	ClusterProfileAccessConfig *access.Config

	// Optional test/customization seams. If unset, the provider uses the
	// repository's default hub resolver, Cluster Inventory remote config
	// resolver, and Kubernetes TokenRequest client.
	HubResolver          HubResolver
	RemoteConfigResolver RemoteConfigResolver
	TokenRequester       TokenRequester

	// Namespace containing AWSWorkloadIdentityConfig/default and
	// AWSServiceAccountRole objects. In OCM this normally maps to the target
	// cluster name.
	WorkloadNamespace string

	// Optional direct role name. If empty, resolve by ServiceAccount.
	AWSServiceAccountRoleName string

	ServiceAccount types.NamespacedName

	// Optional explicit STS region override. If empty, the default provider
	// prefers ClusterProfile status property aws.identity.appthrust.io.aws-region
	// and then falls back to AWSWorkloadIdentityConfig/default spec.region.
	Region          string
	TokenExpiration time.Duration
	SessionDuration time.Duration
	// Required. Passed to STS AssumeRoleWithWebIdentity as RoleSessionName.
	SessionName string

	STSClientFactory func(region string) STSAssumeRoleWithWebIdentityAPI
}

// HubResolver resolves hub-side AWS identity API objects.
type HubResolver interface {
	Resolve(ctx context.Context, opts ResolveOptions) (ResolvedRole, error)
}

// ResolveOptions configures hub-side identity resolution.
type ResolveOptions struct {
	WorkloadNamespace         string
	AWSServiceAccountRoleName string
	ServiceAccount            types.NamespacedName
	RegionOverride            string
}

// ResolvedRole is the AWS identity contract resolved from hub-side API
// objects.
type ResolvedRole struct {
	WorkloadNamespace string
	ConfigRef         types.NamespacedName
	RoleRef           types.NamespacedName
	ServiceAccount    types.NamespacedName
	RoleARN           string
	Region            string
	DeliveryType      string
}

// RemoteConfigResolver resolves the remote Kubernetes API rest.Config from
// Cluster Inventory access providers.
type RemoteConfigResolver interface {
	ResolveRemoteConfig(
		ctx context.Context,
		workloadNamespace string,
		accessConfig *access.Config,
	) (*rest.Config, ResolvedClusterProfile, error)
}

// ResolvedClusterProfile identifies the ClusterProfile and access provider used
// for remote Kubernetes API access.
type ResolvedClusterProfile struct {
	Ref          types.NamespacedName
	ProviderName string
	AWSRegion    string
}

// TokenRequester creates remote ServiceAccount tokens.
type TokenRequester interface {
	RequestServiceAccountToken(
		ctx context.Context,
		restConfig *rest.Config,
		serviceAccount types.NamespacedName,
		audience string,
		expiration time.Duration,
	) (string, error)
}

func (o *Options) setDefaults() {
	if o.TokenExpiration == 0 {
		o.TokenExpiration = defaultTokenExpiration
	}

	if o.STSClientFactory == nil {
		o.STSClientFactory = NewSTSClient
	}

	if o.HubResolver == nil && o.HubReader != nil {
		o.HubResolver = NewHubResolver(o.HubReader)
	}

	if o.RemoteConfigResolver == nil && o.HubReader != nil {
		o.RemoteConfigResolver = NewRemoteConfigResolver(o.HubReader)
	}

	if o.TokenRequester == nil {
		o.TokenRequester = NewTokenRequester()
	}
}

func (o *Options) validate() error { //nolint:cyclop // Validation is an explicit list of independent option guards.
	ctx := errorContext{
		workloadNamespace: o.WorkloadNamespace,
		serviceAccount:    o.ServiceAccount,
	}

	switch {
	case o.WorkloadNamespace == "":
		return newError(ReasonInvalidOptions, "WorkloadNamespace is required", nil, ctx)
	case o.ServiceAccount.Namespace == "" || o.ServiceAccount.Name == "":
		return newError(ReasonInvalidOptions, "ServiceAccount namespace and name are required", nil, ctx)
	case o.SessionName == "":
		return newError(ReasonInvalidOptions, "SessionName is required", nil, ctx)
	case o.HubResolver == nil:
		return newError(ReasonInvalidOptions, "HubReader or HubResolver is required", nil, ctx)
	case o.RemoteConfigResolver == nil:
		return newError(ReasonInvalidOptions, "HubReader or RemoteConfigResolver is required", nil, ctx)
	case o.TokenRequester == nil:
		return newError(ReasonInvalidOptions, "TokenRequester is required", nil, ctx)
	case o.STSClientFactory == nil:
		return newError(ReasonInvalidOptions, "STSClientFactory is required", nil, ctx)
	case o.TokenExpiration <= 0:
		return newError(ReasonInvalidOptions, "TokenExpiration must be greater than zero", nil, ctx)
	case o.TokenExpiration < time.Second:
		return newError(ReasonInvalidOptions, "TokenExpiration must be at least one second", nil, ctx)
	case o.SessionDuration < 0:
		return newError(ReasonInvalidOptions, "SessionDuration cannot be negative", nil, ctx)
	case o.SessionDuration > 0 && o.SessionDuration < minSTSSessionDuration:
		return newError(ReasonInvalidOptions, fmt.Sprintf("SessionDuration must be at least %s when set", minSTSSessionDuration), nil, ctx)
	case o.SessionDuration > maxSTSSessionDuration:
		return newError(ReasonInvalidOptions, fmt.Sprintf("SessionDuration must be at most %s", maxSTSSessionDuration), nil, ctx)
	default:
		return nil
	}
}

var _ aws.CredentialsProvider = (*Provider)(nil)
