package remoteirsa

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// NewCachedProvider returns Provider wrapped in the AWS SDK credentials cache.
func NewCachedProvider(opts Options) (aws.CredentialsProvider, error) { //nolint:gocritic // Public constructor keeps a value options API.
	provider, err := NewProvider(opts)
	if err != nil {
		return nil, fmt.Errorf("create remote IRSA provider: %w", err)
	}

	return aws.NewCredentialsCache(provider), nil
}
