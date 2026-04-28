package aws

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
)

const testIssuerBucket = "issuer-bucket"

type putObjectCall struct {
	bucket      string
	key         string
	contentType string
	body        string
	metadata    map[string]string
}

type headObjectCall struct {
	bucket string
	key    string
}

type fakeS3ObjectClient struct {
	mu          sync.Mutex
	headOutputs map[string]*s3.HeadObjectOutput
	headErrs    map[string]error
	headCalls   []headObjectCall
	deleteCalls []putObjectCall
	putCalls    []putObjectCall
}

func (f *fakeS3ObjectClient) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := awssdk.ToString(input.Key)
	f.headCalls = append(f.headCalls, headObjectCall{
		bucket: awssdk.ToString(input.Bucket),
		key:    key,
	})

	if err := f.headErrs[key]; err != nil {
		return nil, err
	}

	if output := f.headOutputs[key]; output != nil {
		return output, nil
	}

	return nil, &s3types.NotFound{}
}

func (f *fakeS3ObjectClient) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, fmt.Errorf("read PutObject body: %w", err)
	}

	f.mu.Lock()
	f.putCalls = append(f.putCalls, putObjectCall{
		bucket:      awssdk.ToString(input.Bucket),
		key:         awssdk.ToString(input.Key),
		contentType: awssdk.ToString(input.ContentType),
		body:        string(body),
		metadata:    input.Metadata,
	})
	f.mu.Unlock()

	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3ObjectClient) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	f.deleteCalls = append(f.deleteCalls, putObjectCall{
		bucket: awssdk.ToString(input.Bucket),
		key:    awssdk.ToString(input.Key),
	})
	f.mu.Unlock()

	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3ObjectClient) findPutCall(key string) (putObjectCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, call := range f.putCalls {
		if call.key == key {
			return call, true
		}
	}

	return putObjectCall{}, false
}

func (f *fakeS3ObjectClient) findDeleteCall(key string) (putObjectCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, call := range f.deleteCalls {
		if call.key == key {
			return call, true
		}
	}

	return putObjectCall{}, false
}

func TestS3OIDCIssuerPublisherHeadMatchAvoidsPut(t *testing.T) {
	publication := testIssuerPublication(t)
	client := &fakeS3ObjectClient{headOutputs: currentHeadObjects(publication)}

	publisher := NewS3OIDCIssuerPublisher(client)

	changed, err := publisher.EnsureOIDCIssuer(context.Background(), testIssuerBucket, publication)
	if err != nil {
		t.Fatal(err)
	}

	if changed {
		t.Fatal("expected matching S3 metadata to avoid changes")
	}

	if len(client.headCalls) != 2 {
		t.Fatalf("expected two HeadObject calls, got %d", len(client.headCalls))
	}

	if len(client.putCalls) != 0 {
		t.Fatalf("expected no PutObject calls, got %#v", client.putCalls)
	}
}

func TestS3OIDCIssuerPublisherMissingMetadataTriggersPut(t *testing.T) {
	publication := testIssuerPublication(t)
	headOutputs := currentHeadObjects(publication)
	headOutputs[oidc.JWKSObjectKey].Metadata = map[string]string{}
	client := &fakeS3ObjectClient{headOutputs: headOutputs}

	publisher := NewS3OIDCIssuerPublisher(client)

	changed, err := publisher.EnsureOIDCIssuer(context.Background(), testIssuerBucket, publication)
	if err != nil {
		t.Fatal(err)
	}

	if !changed {
		t.Fatal("expected stale metadata to be rewritten")
	}

	if len(client.putCalls) != 1 {
		t.Fatalf("expected one PutObject call, got %#v", client.putCalls)
	}

	if client.putCalls[0].key != oidc.JWKSObjectKey {
		t.Fatalf("expected JWKS object to be rewritten, got %#v", client.putCalls[0])
	}
}

func TestS3OIDCIssuerPublisherPutsExpectedMetadataAndContentType(t *testing.T) {
	publication := testIssuerPublication(t)
	client := &fakeS3ObjectClient{}

	publisher := NewS3OIDCIssuerPublisher(client)

	changed, err := publisher.EnsureOIDCIssuer(context.Background(), testIssuerBucket, publication)
	if err != nil {
		t.Fatal(err)
	}

	if !changed {
		t.Fatal("expected missing objects to be written")
	}

	if len(client.putCalls) != 2 {
		t.Fatalf("expected two PutObject calls, got %d", len(client.putCalls))
	}

	discovery, ok := client.findPutCall(oidc.DiscoveryObjectKey)
	if !ok {
		t.Fatalf("expected discovery PutObject call, got %#v", client.putCalls)
	}

	if discovery.bucket != testIssuerBucket || discovery.contentType != "application/json" {
		t.Fatalf("unexpected discovery PutObject input: %#v", discovery)
	}

	assertIssuerObjectPutMetadata(t, discovery, publication, oidc.DiscoveryObjectKey)

	if discovery.body == "" {
		t.Fatalf("unexpected discovery body: %s", discovery.body)
	}

	jwks, ok := client.findPutCall(oidc.JWKSObjectKey)
	if !ok {
		t.Fatalf("expected JWKS PutObject call, got %#v", client.putCalls)
	}

	if jwks.bucket != testIssuerBucket || jwks.contentType != "application/json" {
		t.Fatalf("unexpected JWKS PutObject input: %#v", jwks)
	}

	assertIssuerObjectPutMetadata(t, jwks, publication, oidc.JWKSObjectKey)

	if jwks.body == "" {
		t.Fatalf("unexpected JWKS body: %s", jwks.body)
	}
}

func TestS3OIDCIssuerPublisherDeletesDiscoveryAndJWKSObjects(t *testing.T) {
	client := &fakeS3ObjectClient{}

	publisher := NewS3OIDCIssuerPublisher(client)
	if err := publisher.DeleteOIDCIssuer(context.Background(), testIssuerBucket); err != nil {
		t.Fatal(err)
	}

	if len(client.deleteCalls) != 2 {
		t.Fatalf("expected two DeleteObject calls, got %d", len(client.deleteCalls))
	}

	discovery, ok := client.findDeleteCall(oidc.DiscoveryObjectKey)
	if !ok || discovery.bucket != testIssuerBucket {
		t.Fatalf("expected discovery DeleteObject call, got %#v", client.deleteCalls)
	}

	jwks, ok := client.findDeleteCall(oidc.JWKSObjectKey)
	if !ok || jwks.bucket != testIssuerBucket {
		t.Fatalf("expected JWKS DeleteObject call, got %#v", client.deleteCalls)
	}
}

func testIssuerPublication(t *testing.T) oidc.IssuerPublication {
	t.Helper()

	_, pub, keyID, err := oidc.GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	publication, err := oidc.RenderIssuerPublication("https://issuer.example.com", pub, keyID)
	if err != nil {
		t.Fatal(err)
	}

	return publication
}

func currentHeadObjects(publication oidc.IssuerPublication) map[string]*s3.HeadObjectOutput {
	out := make(map[string]*s3.HeadObjectOutput, len(publication.Objects))

	for _, object := range publication.Objects {
		out[object.Key] = &s3.HeadObjectOutput{
			ContentType: awssdk.String(object.ContentType),
			Metadata: map[string]string{
				s3MetadataPublicationFormat: oidc.IssuerPublicationFormat,
				s3MetadataObjectDigest:      object.ObjectDigest,
				s3MetadataObjectSetDigest:   publication.ObjectSetDigest,
			},
		}
	}

	return out
}

func assertIssuerObjectPutMetadata(t *testing.T, call putObjectCall, publication oidc.IssuerPublication, key string) {
	t.Helper()

	var object oidc.IssuerObject

	for _, candidate := range publication.Objects {
		if candidate.Key == key {
			object = candidate

			break
		}
	}

	if object.Key == "" {
		t.Fatalf("test publication missing object %q", key)
	}

	if call.metadata[s3MetadataPublicationFormat] != oidc.IssuerPublicationFormat ||
		call.metadata[s3MetadataObjectDigest] != object.ObjectDigest ||
		call.metadata[s3MetadataObjectSetDigest] != publication.ObjectSetDigest {
		t.Fatalf("unexpected PutObject metadata for %s: %#v", key, call.metadata)
	}
}
