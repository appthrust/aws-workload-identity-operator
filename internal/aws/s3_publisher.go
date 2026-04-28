package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"

	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
)

// S3ObjectAPI is the subset of the AWS S3 client used by the OIDC publisher.
type S3ObjectAPI interface {
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3OIDCIssuerPublisher publishes self-hosted OIDC issuer documents to S3.
type S3OIDCIssuerPublisher struct {
	Client S3ObjectAPI
}

// NewS3OIDCIssuerPublisher creates an S3-backed OIDC issuer publisher.
func NewS3OIDCIssuerPublisher(client S3ObjectAPI) *S3OIDCIssuerPublisher {
	return &S3OIDCIssuerPublisher{Client: client}
}

// PublishOIDCIssuer writes the OpenID configuration and JWKS objects.
func (p *S3OIDCIssuerPublisher) PublishOIDCIssuer(ctx context.Context, bucket, issuerURL string, publicKeyPEM []byte, keyID string) error {
	if bucket == "" {
		return fmt.Errorf("bucket is empty")
	}

	objects, err := oidc.RenderIssuerObjects(issuerURL, publicKeyPEM, keyID)
	if err != nil {
		return fmt.Errorf("render OIDC issuer objects: %w", err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	for _, object := range objects {
		g.Go(func() error {
			if _, err := p.Client.PutObject(gCtx, &s3.PutObjectInput{
				Bucket:      awssdk.String(bucket),
				Key:         awssdk.String(object.Key),
				Body:        bytes.NewReader(object.Body),
				ContentType: awssdk.String(object.ContentType),
			}); err != nil {
				return fmt.Errorf("put S3 object s3://%s/%s: %w", bucket, object.Key, err)
			}

			return nil
		})
	}

	return g.Wait() //nolint:wrapcheck // child errors already wrapped with object key
}

// DeleteOIDCIssuer removes the OpenID configuration and JWKS objects.
func (p *S3OIDCIssuerPublisher) DeleteOIDCIssuer(ctx context.Context, bucket string) error {
	if bucket == "" {
		return fmt.Errorf("bucket is empty")
	}

	g, gCtx := errgroup.WithContext(ctx)

	for _, key := range []string{oidc.DiscoveryObjectKey, oidc.JWKSObjectKey} {
		g.Go(func() error {
			if _, err := p.Client.DeleteObject(gCtx, &s3.DeleteObjectInput{
				Bucket: awssdk.String(bucket),
				Key:    awssdk.String(key),
			}); err != nil && !errors.As(err, new(*s3types.NoSuchBucket)) {
				return fmt.Errorf("delete S3 object s3://%s/%s: %w", bucket, key, err)
			}

			return nil
		})
	}

	return g.Wait() //nolint:wrapcheck // child errors already wrapped with object key
}
