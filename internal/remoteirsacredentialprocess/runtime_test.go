package remoteirsacredentialprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
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

	var stdout bytes.Buffer
	if err := WriteCredentialProcessOutput(&stdout, &creds); err != nil {
		t.Fatalf("WriteCredentialProcessOutput returned error: %v", err)
	}

	var got CredentialProcessOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}

	if got.Version != 1 {
		t.Fatalf("Version = %d, want 1", got.Version)
	}

	if got.AccessKeyID != "AKIAEXAMPLE" {
		t.Fatalf("AccessKeyId = %q", got.AccessKeyID)
	}

	if got.SecretAccessKey != "secret" {
		t.Fatalf("SecretAccessKey = %q", got.SecretAccessKey)
	}

	if got.SessionToken != "session" {
		t.Fatalf("SessionToken = %q", got.SessionToken)
	}

	if got.Expiration != "2026-05-04T03:34:56Z" {
		t.Fatalf("Expiration = %q", got.Expiration)
	}
}

func TestRunWritesCredentialProcessJSON(t *testing.T) {
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

	opts := testRuntimeOptions(t)

	code := Run(context.Background(), &opts, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}

	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var got CredentialProcessOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}

	if got.AccessKeyID != "AKIAEXAMPLE" || got.SecretAccessKey != "secret" || got.SessionToken != "session" {
		t.Fatalf("unexpected credential_process output: %#v", got)
	}
}

func TestRunWritesErrorsOnlyToStderr(t *testing.T) {
	deps := testDependencies(t, &fakeProvider{err: errors.New("sts access denied")})

	var stdout, stderr bytes.Buffer

	opts := testRuntimeOptions(t)

	code := Run(context.Background(), &opts, &stdout, &stderr, deps)
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

	opts := testRuntimeOptions(t)

	code := Run(context.Background(), &opts, &stdout, &stderr, deps)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}

	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	if strings.Contains(stderr.String(), "secret") {
		t.Fatalf("stderr leaked secret-bearing error: %q", stderr.String())
	}

	if got := strings.TrimSpace(stderr.String()); got != sanitizedCredentialProcessFailure {
		t.Fatalf("stderr = %q", got)
	}
}

func TestSanitizeErrorRedactsSecretBearingErrors(t *testing.T) {
	tests := []string{
		"Secret Access Key=secret",
		"Session Token: session",
		"Web Identity Token token",
		"AWS_SECRET_ACCESS_KEY=secret",
		"Authorization: Bearer token",
		"Access-Key-Id=AKIAEXAMPLE",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			if got := SanitizeError(errors.New(tt)); got != sanitizedCredentialProcessFailure {
				t.Fatalf("SanitizeError() = %q", got)
			}
		})
	}
}

func TestSanitizeErrorLeavesNonSecretErrorsUnchanged(t *testing.T) {
	err := errors.New("sts access denied")
	if got := SanitizeError(err); got != err.Error() {
		t.Fatalf("SanitizeError() = %q, want %q", got, err.Error())
	}
}

func TestRunReportsMissingDependency(t *testing.T) {
	var stdout, stderr bytes.Buffer

	opts := testRuntimeOptions(t)

	code := Run(context.Background(), &opts, &stdout, &stderr, Dependencies{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}

	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	if !strings.Contains(stderr.String(), "BuildAccessConfig dependency is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func testRuntimeOptions(t *testing.T) Options {
	t.Helper()

	providerFile := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(providerFile, []byte(`{"providers":[{"name":"open-cluster-management"}]}`), 0o600); err != nil {
		t.Fatalf("write provider file: %v", err)
	}

	return Options{
		Access: AccessConfigOptions{
			ProviderFile: providerFile,
		},
		WorkloadNamespace: "wlc-a",
		ServiceAccount: types.NamespacedName{
			Namespace: "app",
			Name:      "workload",
		},
		AWSServiceAccountRoleName: "role-a",
		ClusterProfileNamespaces:  []string{"awio-system"},
		Region:                    "us-east-1",
		TokenExpiration:           10 * time.Minute,
		SessionDuration:           30 * time.Minute,
		SessionName:               "app-workload",
	}
}

func testDependencies(t *testing.T, provider *fakeProvider) Dependencies {
	t.Helper()

	hubReader := ctrlfake.NewClientBuilder().WithScheme(HubScheme()).Build()
	hubKubeClient := kubernetesfake.NewSimpleClientset()

	return Dependencies{
		BuildAccessConfig: BuildAccessConfig,
		BuildHubConfig: func(string) (*rest.Config, error) {
			return &rest.Config{Host: "https://hub.example.com"}, nil
		},
		NewHubClient: func(*rest.Config) (client.Client, error) {
			return hubReader, nil
		},
		NewHubKubeClient: func(*rest.Config) (kubernetes.Interface, error) {
			return hubKubeClient, nil
		},
		NewProvider: func(opts remoteirsa.Options) (CredentialProvider, error) {
			if opts.ClusterProfileAccessConfig == nil {
				t.Fatal("ClusterProfileAccessConfig is nil")
			}

			if !slices.Equal(opts.ClusterProfileNamespaces, []string{"awio-system"}) {
				t.Fatalf("ClusterProfileNamespaces = %#v, want %#v", opts.ClusterProfileNamespaces, []string{"awio-system"})
			}

			if opts.HubReader != hubReader {
				t.Fatalf("HubReader = %#v, want %#v", opts.HubReader, hubReader)
			}

			if opts.HubKubeClient != hubKubeClient {
				t.Fatalf("HubKubeClient = %#v, want %#v", opts.HubKubeClient, hubKubeClient)
			}

			if opts.WorkloadNamespace != "wlc-a" {
				t.Fatalf("WorkloadNamespace = %q", opts.WorkloadNamespace)
			}

			if opts.AWSServiceAccountRoleName != "role-a" {
				t.Fatalf("AWSServiceAccountRoleName = %q", opts.AWSServiceAccountRoleName)
			}

			if opts.ServiceAccount != (types.NamespacedName{Namespace: "app", Name: "workload"}) {
				t.Fatalf("ServiceAccount = %#v", opts.ServiceAccount)
			}

			if opts.Region != "us-east-1" {
				t.Fatalf("Region = %q", opts.Region)
			}

			if opts.TokenExpiration != 10*time.Minute {
				t.Fatalf("TokenExpiration = %s", opts.TokenExpiration)
			}

			if opts.SessionDuration != 30*time.Minute {
				t.Fatalf("SessionDuration = %s", opts.SessionDuration)
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
