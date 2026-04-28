package remoteirsa

import (
	"cmp"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

// Provider implements aws.CredentialsProvider using remote Kubernetes
// TokenRequest and STS AssumeRoleWithWebIdentity.
type Provider struct {
	opts Options
}

// NewProvider validates options and stores dependencies. It does not read hub
// APIs, build remote clients, request tokens, or call STS.
func NewProvider(opts Options) (*Provider, error) { //nolint:gocritic // Public constructor keeps a value options API.
	opts.setDefaults()

	if err := opts.validate(); err != nil {
		return nil, err
	}

	return &Provider{opts: opts}, nil
}

// Retrieve resolves the hub-side role contract, requests a fresh remote web
// identity token, exchanges it with STS, and returns AWS credentials.
func (p *Provider) Retrieve(ctx context.Context) (aws.Credentials, error) { //nolint:funlen // Linear credential exchange is clearer in one flow.
	resolved, err := p.opts.HubResolver.Resolve(ctx, ResolveOptions{
		WorkloadNamespace:         p.opts.WorkloadNamespace,
		AWSServiceAccountRoleName: p.opts.AWSServiceAccountRoleName,
		ServiceAccount:            p.opts.ServiceAccount,
		RegionOverride:            p.opts.Region,
	})
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("resolve remote IRSA role: %w", err)
	}

	errCtx := errorContext{
		workloadNamespace: resolved.WorkloadNamespace,
		serviceAccount:    resolved.ServiceAccount,
		roleRef:           resolved.RoleRef,
	}

	if !identityv1.DeliveryType(resolved.DeliveryType).UsesAnnotationBasedIRSA() {
		return aws.Credentials{}, newError(
			ReasonUnsupportedDeliveryType,
			fmt.Sprintf("delivery type %q is not supported by remote IRSA", resolved.DeliveryType),
			nil,
			errCtx,
		)
	}

	remoteConfig, clusterProfile, err := p.opts.RemoteConfigResolver.ResolveRemoteConfig(
		ctx,
		resolved.WorkloadNamespace,
		p.opts.ClusterProfileAccessConfig,
	)
	errCtx.clusterProfileRef = clusterProfile.Ref
	errCtx.providerName = clusterProfile.ProviderName

	if err != nil {
		return aws.Credentials{}, withErrorContext(err, errCtx)
	}

	region := cmp.Or(p.opts.Region, clusterProfile.AWSRegion, resolved.Region)
	if region == "" {
		return aws.Credentials{}, newError(ReasonRegionNotReady, "AWS region is empty", nil, errCtx)
	}

	token, err := p.opts.TokenRequester.RequestServiceAccountToken(
		ctx,
		remoteConfig,
		resolved.ServiceAccount,
		STSAudience,
		p.opts.TokenExpiration,
	)
	if err != nil {
		return aws.Credentials{}, withErrorContext(classifyTokenRequestError(err), errCtx)
	}

	stsClient := p.opts.STSClientFactory(region)
	if stsClient == nil {
		return aws.Credentials{}, newError(ReasonInvalidOptions, "STSClientFactory returned nil", nil, errCtx)
	}

	creds, err := assumeRoleWithWebIdentity(
		ctx,
		stsClient,
		resolved.RoleARN,
		token,
		p.opts.SessionName,
		p.opts.SessionDuration,
	)
	if err != nil {
		return aws.Credentials{}, withErrorContext(err, errCtx)
	}

	return creds, nil
}

