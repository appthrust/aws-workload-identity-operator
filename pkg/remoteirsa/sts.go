package remoteirsa

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
)

// STSAssumeRoleWithWebIdentityAPI is the STS operation used by Provider.
type STSAssumeRoleWithWebIdentityAPI interface {
	AssumeRoleWithWebIdentity(ctx context.Context, params *sts.AssumeRoleWithWebIdentityInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error)
}

// NewSTSClient returns an AWS SDK v2 STS client for region. STS
// AssumeRoleWithWebIdentity does not require signing credentials.
func NewSTSClient(region string) STSAssumeRoleWithWebIdentityAPI {
	return sts.New(sts.Options{
		Region:      region,
		Credentials: aws.AnonymousCredentials{},
	})
}

func assumeRoleWithWebIdentity(
	ctx context.Context,
	client STSAssumeRoleWithWebIdentityAPI,
	roleARN string,
	webIdentityToken string,
	sessionName string,
	sessionDuration time.Duration,
) (aws.Credentials, error) {
	input := &sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          aws.String(roleARN),
		RoleSessionName:  aws.String(sessionName),
		WebIdentityToken: aws.String(webIdentityToken),
	}

	if sessionDuration > 0 {
		duration := int64(sessionDuration / time.Second)
		if duration > math.MaxInt32 {
			return aws.Credentials{}, newError(ReasonInvalidOptions, "SessionDuration is too large for STS DurationSeconds", nil, errorContext{})
		}

		input.DurationSeconds = aws.Int32(int32(duration)) //nolint:gosec // Bound checked against math.MaxInt32 above.
	}

	output, err := client.AssumeRoleWithWebIdentity(ctx, input)
	if err != nil {
		return aws.Credentials{}, classifySTSError(err)
	}

	if output == nil || output.Credentials == nil {
		return aws.Credentials{}, newError(ReasonSTSCredentialsUnavailable, "STS response did not include credentials", nil, errorContext{})
	}

	creds := output.Credentials
	if creds.AccessKeyId == nil || creds.SecretAccessKey == nil || creds.SessionToken == nil || creds.Expiration == nil {
		return aws.Credentials{}, newError(ReasonSTSCredentialsUnavailable, "STS response included incomplete credentials", nil, errorContext{})
	}

	return aws.Credentials{
		AccessKeyID:     aws.ToString(creds.AccessKeyId),
		SecretAccessKey: aws.ToString(creds.SecretAccessKey),
		SessionToken:    aws.ToString(creds.SessionToken),
		Source:          "RemoteIRSAAssumeRoleWithWebIdentity",
		CanExpire:       true,
		Expires:         aws.ToTime(creds.Expiration),
		AccountID:       accountIDFromAssumedRoleUser(output.AssumedRoleUser),
	}, nil
}

func classifySTSError(err error) error {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return newError(ReasonSTSAssumeRoleWithWebIdentity, "STS AssumeRoleWithWebIdentity failed", err, errorContext{})
	}

	switch apiErr.ErrorCode() {
	case "AccessDenied", "AccessDeniedException":
		return newError(ReasonSTSAccessDenied, "STS AssumeRoleWithWebIdentity was denied", err, errorContext{})
	case "ExpiredToken", "ExpiredTokenException", "InvalidIdentityToken", "InvalidIdentityTokenException", "IDPRejectedClaim":
		return newError(ReasonExpiredOrInvalidToken, "STS rejected the web identity token", err, errorContext{})
	default:
		return newError(ReasonSTSAssumeRoleWithWebIdentity, "STS AssumeRoleWithWebIdentity failed", err, errorContext{})
	}
}

func accountIDFromAssumedRoleUser(user *ststypes.AssumedRoleUser) string {
	if user == nil || user.Arn == nil {
		return ""
	}

	parsed, err := arn.Parse(*user.Arn)
	if err != nil {
		return ""
	}

	return parsed.AccountID
}
