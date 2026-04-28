package controller

import (
	"strconv"

	identityaws "github.com/appthrust/aws-workload-identity-operator/internal/aws"
)

// Annotation keys consumed by aws-pod-identity-webhook on remote workload
// ServiceAccounts. The webhook contract is stable; copying the constants here
// avoids depending on the upstream Go module just for four strings.
const (
	selfHostedAnnotationPrefix = "eks.amazonaws.com"

	selfHostedRoleARNAnnotation         = selfHostedAnnotationPrefix + "/role-arn"
	selfHostedAudienceAnnotation        = selfHostedAnnotationPrefix + "/audience"
	selfHostedRegionalSTSAnnotation     = selfHostedAnnotationPrefix + "/sts-regional-endpoints"
	selfHostedTokenExpirationAnnotation = selfHostedAnnotationPrefix + "/token-expiration"

	// selfHostedSkipWebhookLabel is the Pod label that opts out of webhook
	// mutation. Hard-coded by the upstream aws-pod-identity-webhook binary; do
	// NOT derive from selfHostedAnnotationPrefix because a future white-label
	// override of the prefix would silently break the skip path.
	selfHostedSkipWebhookLabel = "eks.amazonaws.com/skip-pod-identity-webhook" //nolint:gosec // G101: not a credential, this is a public webhook opt-out label key

	selfHostedTokenExpirationSeconds int64 = 86400
)

// renderSelfHostedServiceAccountAnnotations renders aws-pod-identity-webhook
// annotations for one remote workload ServiceAccount.
func renderSelfHostedServiceAccountAnnotations(roleARN string) map[string]string {
	if roleARN == "" {
		return nil
	}

	return map[string]string{
		selfHostedRoleARNAnnotation:         roleARN,
		selfHostedAudienceAnnotation:        identityaws.STSAudience,
		selfHostedRegionalSTSAnnotation:     "true",
		selfHostedTokenExpirationAnnotation: strconv.FormatInt(selfHostedTokenExpirationSeconds, 10),
	}
}

// selfHostedServiceAccountAnnotationKeys returns the annotations managed by the
// operator on remote workload ServiceAccounts.
func selfHostedServiceAccountAnnotationKeys() []string {
	return []string{
		selfHostedRoleARNAnnotation,
		selfHostedAudienceAnnotation,
		selfHostedRegionalSTSAnnotation,
		selfHostedTokenExpirationAnnotation,
	}
}
