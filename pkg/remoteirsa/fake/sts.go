// Package fake provides test fakes for remoteirsa dependencies.
package fake

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// STSClient is a test fake for STS AssumeRoleWithWebIdentity.
type STSClient struct {
	Calls  []AssumeRoleWithWebIdentityCall
	Output *sts.AssumeRoleWithWebIdentityOutput
	Err    error
}

// AssumeRoleWithWebIdentityCall records one STS call.
type AssumeRoleWithWebIdentityCall struct {
	Input *sts.AssumeRoleWithWebIdentityInput
}

// AssumeRoleWithWebIdentity records the call and returns the configured output or error.
func (c *STSClient) AssumeRoleWithWebIdentity(_ context.Context, params *sts.AssumeRoleWithWebIdentityInput, _ ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	c.Calls = append(c.Calls, AssumeRoleWithWebIdentityCall{Input: params})
	if c.Err != nil {
		return nil, c.Err
	}

	return c.Output, nil
}
