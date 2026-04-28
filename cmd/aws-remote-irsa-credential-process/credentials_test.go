package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWriteCredentialProcessOutput(t *testing.T) {
	expires := time.Date(2026, 5, 4, 12, 34, 56, 0, time.FixedZone("JST", 9*60*60))
	creds := awssdk.Credentials{
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "secret",
		SessionToken:    "session",
		CanExpire:       true,
		Expires:         expires,
	}

	var out bytes.Buffer
	if err := writeCredentialProcessOutput(&out, &creds); err != nil {
		t.Fatalf("writeCredentialProcessOutput returned error: %v", err)
	}

	var got credentialProcessOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}

	if got.Version != 1 {
		t.Fatalf("Version = %d, want 1", got.Version)
	}

	if got.AccessKeyID != creds.AccessKeyID {
		t.Fatalf("AccessKeyId = %q", got.AccessKeyID)
	}

	if got.SecretAccessKey != creds.SecretAccessKey {
		t.Fatalf("SecretAccessKey = %q", got.SecretAccessKey)
	}

	if got.SessionToken != creds.SessionToken {
		t.Fatalf("SessionToken = %q", got.SessionToken)
	}

	if got.Expiration != "2026-05-04T03:34:56Z" {
		t.Fatalf("Expiration = %q", got.Expiration)
	}
}

func TestRunWritesOnlyJSONToStdout(t *testing.T) {
	deps := testDependencies(t, &fakeProvider{
		creds: awssdk.Credentials{
			AccessKeyID:     "AKIAEXAMPLE",
			SecretAccessKey: "secret",
			SessionToken:    "session",
			CanExpire:       true,
			Expires:         time.Date(2026, 5, 4, 12, 34, 56, 0, time.UTC),
		},
	})

	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{
		"--namespace", "wlc-a",
		"--service-account", "app/workload",
		"--clusterprofile-provider-file", "/tmp/providers.json",
		"--session-name", "app-workload",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var got credentialProcessOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not credential_process JSON: %v; stdout=%q", err, stdout.String())
	}

	if got.AccessKeyID != "AKIAEXAMPLE" {
		t.Fatalf("AccessKeyId = %q", got.AccessKeyID)
	}
}

func TestRunWritesErrorsOnlyToStderr(t *testing.T) {
	deps := testDependencies(t, &fakeProvider{err: errors.New("sts access denied")})

	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{
		"--namespace", "wlc-a",
		"--service-account", "app/workload",
		"--clusterprofile-provider-file", "/tmp/providers.json",
		"--session-name", "app-workload",
	}, &stdout, &stderr, deps)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}

	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	if !strings.Contains(stderr.String(), "retrieve remote IRSA credentials: sts access denied") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRedactsSecretBearingErrors(t *testing.T) {
	deps := testDependencies(t, &fakeProvider{err: errors.New("SecretAccessKey=secret")})

	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{
		"--namespace", "wlc-a",
		"--service-account", "app/workload",
		"--clusterprofile-provider-file", "/tmp/providers.json",
		"--session-name", "app-workload",
	}, &stdout, &stderr, deps)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}

	if strings.Contains(stderr.String(), "secret") {
		t.Fatalf("stderr leaked secret-bearing error: %q", stderr.String())
	}

	if got := strings.TrimSpace(stderr.String()); got != "remote IRSA credential_process failed" {
		t.Fatalf("stderr = %q", got)
	}
}

func testDependencies(t *testing.T, provider *fakeProvider) dependencies {
	t.Helper()

	return dependencies{
		loadAccessConfig: func(path string) (*access.Config, error) {
			if path == "" {
				t.Fatal("provider file path was empty")
			}

			return &access.Config{}, nil
		},
		buildHubConfig: func(_ string) (*rest.Config, error) {
			return &rest.Config{Host: "https://hub.example.com"}, nil
		},
		newHubClient: func(_ *rest.Config) (client.Client, error) {
			return nil, nil
		},
		newHubKubeClient: func(_ *rest.Config) (kubernetes.Interface, error) {
			return nil, nil
		},
		newProvider: func(opts remoteIRSAOptions) (credentialProvider, error) {
			if opts.WorkloadNamespace != "wlc-a" {
				t.Fatalf("WorkloadNamespace = %q", opts.WorkloadNamespace)
			}

			if opts.ServiceAccount.Namespace != "app" || opts.ServiceAccount.Name != "workload" {
				t.Fatalf("ServiceAccount = %#v", opts.ServiceAccount)
			}

			if opts.SessionName != "app-workload" {
				t.Fatalf("SessionName = %q", opts.SessionName)
			}

			return provider, nil
		},
	}
}

type fakeProvider struct {
	creds awssdk.Credentials
	err   error
}

func (p *fakeProvider) Retrieve(context.Context) (awssdk.Credentials, error) {
	return p.creds, p.err
}
