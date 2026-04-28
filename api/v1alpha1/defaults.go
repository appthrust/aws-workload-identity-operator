package v1alpha1

// DefaultSelfHostedWebhookNamespace is the default namespace for the self-hosted
// aws-pod-identity-webhook runtime when no override is supplied.
const DefaultSelfHostedWebhookNamespace = "aws-pod-identity-webhook"

// WithDefaults returns a copy of the operator configuration with defaults
// filled in. The receiver is left untouched, so callers can pass cached objects
// without coordinating around mutation.
func (c *AWSWorkloadIdentityOperatorConfig) WithDefaults() *AWSWorkloadIdentityOperatorConfig {
	if c.Spec.SelfHostedIRSA.WebhookNamespace != "" {
		return c
	}

	out := c.DeepCopy()
	out.Spec.SelfHostedIRSA.WebhookNamespace = DefaultSelfHostedWebhookNamespace

	return out
}
