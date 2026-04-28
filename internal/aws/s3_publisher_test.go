package aws

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
)

const testIssuerBucket = "issuer-bucket"

type putObjectCall struct {
	bucket      string
	key         string
	contentType string
	body        string
}

type fakeS3PutObjectClient struct {
	mu          sync.Mutex
	deleteCalls []putObjectCall
	putCalls    []putObjectCall
}

func (f *fakeS3PutObjectClient) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
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
	})
	f.mu.Unlock()

	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3PutObjectClient) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	f.deleteCalls = append(f.deleteCalls, putObjectCall{
		bucket: awssdk.ToString(input.Bucket),
		key:    awssdk.ToString(input.Key),
	})
	f.mu.Unlock()

	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3PutObjectClient) findPutCall(key string) (putObjectCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, call := range f.putCalls {
		if call.key == key {
			return call, true
		}
	}

	return putObjectCall{}, false
}

func (f *fakeS3PutObjectClient) findDeleteCall(key string) (putObjectCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, call := range f.deleteCalls {
		if call.key == key {
			return call, true
		}
	}

	return putObjectCall{}, false
}

func TestS3OIDCIssuerPublisherPutsDiscoveryAndJWKSObjects(t *testing.T) {
	_, pub, keyID, err := oidc.GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	client := &fakeS3PutObjectClient{}

	publisher := NewS3OIDCIssuerPublisher(client)
	if err := publisher.PublishOIDCIssuer(context.Background(), testIssuerBucket, "https://issuer.example.com", pub, keyID); err != nil {
		t.Fatal(err)
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

	if discovery.body == "" || !strings.Contains(discovery.body, `"issuer":"https://issuer.example.com"`) {
		t.Fatalf("unexpected discovery body: %s", discovery.body)
	}

	jwks, ok := client.findPutCall(oidc.JWKSObjectKey)
	if !ok {
		t.Fatalf("expected JWKS PutObject call, got %#v", client.putCalls)
	}

	if jwks.bucket != testIssuerBucket || jwks.contentType != "application/json" {
		t.Fatalf("unexpected JWKS PutObject input: %#v", jwks)
	}

	if jwks.body == "" || !strings.Contains(jwks.body, `"kid":"`+keyID+`"`) {
		t.Fatalf("unexpected JWKS body: %s", jwks.body)
	}
}

func TestS3OIDCIssuerPublisherDeletesDiscoveryAndJWKSObjects(t *testing.T) {
	client := &fakeS3PutObjectClient{}

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
