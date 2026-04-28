package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"

	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
)

// S3ObjectAPI is the subset of the AWS S3 client used by the OIDC publisher.
type S3ObjectAPI interface {
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

const (
	s3MetadataPublicationFormat = "awio-publication-format"
	s3MetadataObjectDigest      = "awio-object-digest"
	s3MetadataObjectSetDigest   = "awio-object-set-digest"
)

// S3OIDCIssuerPublisher publishes self-hosted OIDC issuer documents to S3.
type S3OIDCIssuerPublisher struct {
	Client S3ObjectAPI
}

// NewS3OIDCIssuerPublisher creates an S3-backed OIDC issuer publisher.
func NewS3OIDCIssuerPublisher(client S3ObjectAPI) *S3OIDCIssuerPublisher {
	return &S3OIDCIssuerPublisher{Client: client}
}

// EnsureOIDCIssuer verifies the issuer object metadata in S3 and rewrites any
// object that is missing, stale, or not owned by the current publication.
func (p *S3OIDCIssuerPublisher) EnsureOIDCIssuer(ctx context.Context, bucket string, publication oidc.IssuerPublication) (bool, error) {
	if bucket == "" {
		return false, fmt.Errorf("bucket is empty")
	}

	if publication.ObjectSetDigest == "" {
		return false, fmt.Errorf("publication object set digest is empty")
	}

	if len(publication.Objects) == 0 {
		return false, fmt.Errorf("publication objects are empty")
	}

	for _, object := range publication.Objects {
		if err := validateIssuerObjectForPublish(object); err != nil {
			return false, err
		}
	}

	var changed atomic.Bool

	g, gCtx := errgroup.WithContext(ctx)

	for _, object := range publication.Objects {
		g.Go(func() error {
			current, err := p.Client.HeadObject(gCtx, &s3.HeadObjectInput{
				Bucket: awssdk.String(bucket),
				Key:    awssdk.String(object.Key),
			})
			if err != nil && !isS3ObjectMissing(err) {
				return fmt.Errorf("head S3 object s3://%s/%s: %w", bucket, object.Key, err)
			}

			if err == nil && s3IssuerObjectCurrent(current, object, publication.ObjectSetDigest) {
				return nil
			}

			if err := p.putIssuerObject(gCtx, bucket, object, publication.ObjectSetDigest); err != nil {
				return err
			}

			changed.Store(true)

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return false, err //nolint:wrapcheck // child errors already wrapped with object key
	}

	return changed.Load(), nil
}

func (p *S3OIDCIssuerPublisher) putIssuerObject(ctx context.Context, bucket string, object oidc.IssuerObject, objectSetDigest string) error {
	if _, err := p.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      awssdk.String(bucket),
		Key:         awssdk.String(object.Key),
		Body:        bytes.NewReader(object.Body),
		ContentType: awssdk.String(object.ContentType),
		Metadata: map[string]string{
			s3MetadataPublicationFormat: oidc.IssuerPublicationFormat,
			s3MetadataObjectDigest:      object.ObjectDigest,
			s3MetadataObjectSetDigest:   objectSetDigest,
		},
	}); err != nil {
		return fmt.Errorf("put S3 object s3://%s/%s: %w", bucket, object.Key, err)
	}

	return nil
}

func validateIssuerObjectForPublish(object oidc.IssuerObject) error {
	if object.Key == "" {
		return fmt.Errorf("issuer object key is empty")
	}

	if object.ContentType == "" {
		return fmt.Errorf("issuer object %q content type is empty", object.Key)
	}

	if object.ObjectDigest == "" {
		return fmt.Errorf("issuer object %q digest is empty", object.Key)
	}

	return nil
}

func s3IssuerObjectCurrent(head *s3.HeadObjectOutput, object oidc.IssuerObject, objectSetDigest string) bool {
	if head == nil || awssdk.ToString(head.ContentType) != object.ContentType {
		return false
	}

	return head.Metadata[s3MetadataPublicationFormat] == oidc.IssuerPublicationFormat &&
		head.Metadata[s3MetadataObjectDigest] == object.ObjectDigest &&
		head.Metadata[s3MetadataObjectSetDigest] == objectSetDigest
}

func isS3ObjectMissing(err error) bool {
	return errors.As(err, new(*s3types.NotFound)) || errors.As(err, new(*s3types.NoSuchKey))
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
