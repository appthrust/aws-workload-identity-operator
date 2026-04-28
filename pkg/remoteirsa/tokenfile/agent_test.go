package tokenfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

func TestRefreshInfersServiceAccountAndWritesTokenAndAWSConfig(t *testing.T) {
	fixture := newRefreshSuccessFixture(t)

	next, err := fixture.agent.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}

	if next != time.Minute {
		t.Fatalf("next refresh = %s, want 1m", next)
	}

	if fixture.remoteClient.selfSubjectReviewCalls != 1 {
		t.Fatalf("SelfSubjectReview calls = %d, want 1", fixture.remoteClient.selfSubjectReviewCalls)
	}

	if fixture.remoteClient.getServiceAccountCalls[0] != (types.NamespacedName{Namespace: "app", Name: "workload"}) {
		t.Fatalf("Get ServiceAccount = %s", fixture.remoteClient.getServiceAccountCalls[0])
	}

	if len(fixture.remoteClient.tokenRequests) != 1 {
		t.Fatalf("TokenRequest calls = %d, want 1", len(fixture.remoteClient.tokenRequests))
	}

	tokenRequest := fixture.remoteClient.tokenRequests[0]
	if tokenRequest.serviceAccount != (types.NamespacedName{Namespace: "app", Name: "workload"}) {
		t.Fatalf("TokenRequest ServiceAccount = %s", tokenRequest.serviceAccount)
	}

	if tokenRequest.audience != remoteirsa.STSAudience {
		t.Fatalf("TokenRequest audience = %q, want %q", tokenRequest.audience, remoteirsa.STSAudience)
	}

	if tokenRequest.expiration != tokenExpiration {
		t.Fatalf("TokenRequest expiration = %s, want %s", tokenRequest.expiration, tokenExpiration)
	}

	assertRefreshFiles(t, &fixture)
}

type refreshSuccessFixture struct {
	tokenFile     string
	awsConfigFile string
	remoteConfig  *rest.Config
	remoteClient  *fakeRemoteClient
	agent         Agent
}

func newRefreshSuccessFixture(t *testing.T) refreshSuccessFixture {
	t.Helper()

	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "token")
	awsConfigFile := filepath.Join(t.TempDir(), "aws-config")
	remoteConfig := &rest.Config{Host: "https://remote.example.com"}
	remoteClient := &fakeRemoteClient{
		username: "system:serviceaccount:app:workload",
		serviceAccounts: map[types.NamespacedName]*corev1.ServiceAccount{
			{Namespace: "app", Name: "workload"}: workloadServiceAccount(map[string]string{
				remoteirsa.ServiceAccountRoleARNAnnotation:         "arn:aws:iam::123456789012:role/workload",
				remoteirsa.ServiceAccountAudienceAnnotation:        "custom-audience",
				remoteirsa.ServiceAccountRegionalSTSAnnotation:     "true",
				remoteirsa.ServiceAccountTokenExpirationAnnotation: "1200",
			}),
		},
		tokenResponse: TokenResponse{
			Token:               "jwt-token",
			ExpirationTimestamp: now.Add(10 * time.Minute),
		},
	}
	agent := Agent{
		RemoteConfigLoader:  fakeRemoteConfigLoader{cfg: remoteConfig}.Load,
		RemoteClientFactory: fakeRemoteClientFactory{client: remoteClient}.New,
		Now:                 func() time.Time { return now },
		Options: Options{
			Kubeconfig:    "/managed/config/kubeconfig",
			TokenFile:     tokenFile,
			AWSConfigFile: awsConfigFile,
		},
	}

	return refreshSuccessFixture{
		tokenFile:     tokenFile,
		awsConfigFile: awsConfigFile,
		remoteConfig:  remoteConfig,
		remoteClient:  remoteClient,
		agent:         agent,
	}
}

func assertRefreshFiles(t *testing.T, fixture *refreshSuccessFixture) {
	t.Helper()

	tokenData, err := os.ReadFile(fixture.tokenFile)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}

	if string(tokenData) != "jwt-token" {
		t.Fatalf("token file = %q, want jwt-token", string(tokenData))
	}

	awsConfigData, err := os.ReadFile(fixture.awsConfigFile)
	if err != nil {
		t.Fatalf("read AWS config file: %v", err)
	}

	wantAWSConfig := `[default]
role_arn = arn:aws:iam::123456789012:role/workload
web_identity_token_file = ` + fixture.tokenFile + `
role_session_name = ` + roleSessionName(fixture.remoteConfig, types.NamespacedName{Namespace: "app", Name: "workload"}) + `
sts_regional_endpoints = regional
`
	if string(awsConfigData) != wantAWSConfig {
		t.Fatalf("AWS config = %q, want %q", string(awsConfigData), wantAWSConfig)
	}
}

func TestRefreshRejectsNonServiceAccountSelfSubjectReviewUser(t *testing.T) {
	remoteClient := &fakeRemoteClient{username: "jane@example.com"}
	agent := directTestAgent(t, time.Now(), remoteClient)

	_, err := agent.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh returned nil error")
	}

	if !strings.Contains(err.Error(), `cannot infer ServiceAccount from SelfSubjectReview username "jane@example.com"`) {
		t.Fatalf("error = %q", err.Error())
	}

	if len(remoteClient.getServiceAccountCalls) != 0 {
		t.Fatalf("Get ServiceAccount calls = %d, want 0", len(remoteClient.getServiceAccountCalls))
	}
}

func TestRefreshRejectsMissingRoleARNAnnotation(t *testing.T) {
	remoteClient := &fakeRemoteClient{
		username: "system:serviceaccount:app:workload",
		serviceAccounts: map[types.NamespacedName]*corev1.ServiceAccount{
			{Namespace: "app", Name: "workload"}: workloadServiceAccount(nil),
		},
	}
	agent := directTestAgent(t, time.Now(), remoteClient)

	_, err := agent.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh returned nil error")
	}

	want := "remote ServiceAccount app/workload missing eks.amazonaws.com/role-arn annotation"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}

	if len(remoteClient.tokenRequests) != 0 {
		t.Fatalf("TokenRequest calls = %d, want 0", len(remoteClient.tokenRequests))
	}
}

func TestRefreshFallsBackToRetryDelayWhenTokenExpiresBeforeRefreshPoint(t *testing.T) {
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	remoteClient := &fakeRemoteClient{
		username: "system:serviceaccount:app:workload",
		serviceAccounts: map[types.NamespacedName]*corev1.ServiceAccount{
			{Namespace: "app", Name: "workload"}: workloadServiceAccount(map[string]string{
				remoteirsa.ServiceAccountRoleARNAnnotation: "arn:aws:iam::123456789012:role/workload",
			}),
		},
		tokenResponse: TokenResponse{
			Token:               "jwt-token",
			ExpirationTimestamp: now.Add(4 * time.Minute),
		},
	}
	agent := directTestAgent(t, now, remoteClient)

	next, err := agent.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}

	if next != refreshRetryDelay {
		t.Fatalf("next refresh = %s, want %s", next, refreshRetryDelay)
	}

	if remoteClient.tokenRequests[0].expiration != tokenExpiration {
		t.Fatalf("requested expiration = %s, want %s", remoteClient.tokenRequests[0].expiration, tokenExpiration)
	}
}

func TestRunRetriesRefreshErrorsAndKeepsExistingFiles(t *testing.T) {
	remoteClient := &fakeRemoteClient{
		username: "system:serviceaccount:app:workload",
		serviceAccounts: map[types.NamespacedName]*corev1.ServiceAccount{
			{Namespace: "app", Name: "workload"}: workloadServiceAccount(map[string]string{
				remoteirsa.ServiceAccountRoleARNAnnotation: "arn:aws:iam::123456789012:role/workload",
			}),
		},
		tokenResponses: []TokenResponse{
			{Token: "token-a", ExpirationTimestamp: time.Now().Add(20 * time.Minute)},
		},
		tokenErrs: []error{nil, errors.New("request failed")},
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	awsConfigFile := filepath.Join(t.TempDir(), "aws-config")
	sleeper := &recordingSleeper{errs: []error{nil, context.Canceled}}
	agent := Agent{
		RemoteConfigLoader:  fakeRemoteConfigLoader{cfg: &rest.Config{Host: "https://remote.example.com"}}.Load,
		RemoteClientFactory: fakeRemoteClientFactory{client: remoteClient}.New,
		Options: Options{
			Kubeconfig:    "/managed/config/kubeconfig",
			TokenFile:     tokenFile,
			AWSConfigFile: awsConfigFile,
		},
	}

	err := agent.Run(context.Background(), sleeper)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}

	if len(sleeper.durations) != 2 {
		t.Fatalf("sleep calls = %d, want 2", len(sleeper.durations))
	}

	if sleeper.durations[1] != refreshRetryDelay {
		t.Fatalf("second sleep duration = %s, want %s", sleeper.durations[1], refreshRetryDelay)
	}

	if remoteClient.selfSubjectReviewCalls != 1 {
		t.Fatalf("SelfSubjectReview calls = %d, want 1 (cached across iterations)", remoteClient.selfSubjectReviewCalls)
	}

	if len(remoteClient.tokenRequests) != 2 {
		t.Fatalf("TokenRequest calls = %d, want 2 (same client reused across iterations)", len(remoteClient.tokenRequests))
	}

	data, err := os.ReadFile(tokenFile) //nolint:gosec // test reads a temp file path created by this test.
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}

	if string(data) != "token-a" {
		t.Fatalf("token file after failed refresh = %q, want token-a", string(data))
	}

	if _, err := os.Stat(awsConfigFile); err != nil {
		t.Fatalf("stat AWS config after failed refresh: %v", err)
	}
}

func TestRefreshRejectsMissingLocalConfigurationBeforeRemoteCalls(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		wantErr string
	}{
		{
			name: "remote kubeconfig",
			options: Options{
				TokenFile:     filepath.Join(t.TempDir(), "token"),
				AWSConfigFile: filepath.Join(t.TempDir(), "aws-config"),
			},
			wantErr: "kubeconfig is required",
		},
		{
			name: "token file",
			options: Options{
				Kubeconfig:    "/managed/config/kubeconfig",
				AWSConfigFile: filepath.Join(t.TempDir(), "aws-config"),
			},
			wantErr: "token file is required",
		},
		{
			name: "aws config file",
			options: Options{
				Kubeconfig: "/managed/config/kubeconfig",
				TokenFile:  filepath.Join(t.TempDir(), "token"),
			},
			wantErr: "aws config file is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remoteClient := &fakeRemoteClient{}
			agent := Agent{
				RemoteConfigLoader:  fakeRemoteConfigLoader{cfg: &rest.Config{Host: "https://remote.example.com"}}.Load,
				RemoteClientFactory: fakeRemoteClientFactory{client: remoteClient}.New,
				Options:             tt.options,
			}

			_, err := agent.Refresh(context.Background())
			if err == nil {
				t.Fatal("Refresh returned nil error")
			}

			if err.Error() != tt.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tt.wantErr)
			}

			if remoteClient.selfSubjectReviewCalls != 0 {
				t.Fatalf("SelfSubjectReview calls = %d, want 0", remoteClient.selfSubjectReviewCalls)
			}
		})
	}
}

func TestRoleSessionNameIsDeterministicBoundedAndAWSCompatible(t *testing.T) {
	serviceAccount := types.NamespacedName{
		Namespace: "team_system",
		Name:      "controller/name:with:invalid:chars:and:a:very:long:suffix:日本語",
	}
	config := &rest.Config{Host: "https://remote.example.com"}

	first := roleSessionName(config, serviceAccount)
	second := roleSessionName(config, serviceAccount)

	if first != second {
		t.Fatalf("roleSessionName is not deterministic: %q != %q", first, second)
	}

	if len(first) > roleSessionMaxLen {
		t.Fatalf("roleSessionName length = %d, want <= %d: %q", len(first), roleSessionMaxLen, first)
	}

	if !strings.HasPrefix(first, "awio-team_system-controller-name-with-invalid-") {
		t.Fatalf("roleSessionName = %q", first)
	}

	for _, r := range first {
		if r > 127 || !isRoleSessionNameRune(r) {
			t.Fatalf("roleSessionName contains invalid rune %q in %q", r, first)
		}
	}
}

func directTestAgent(t *testing.T, now time.Time, remoteClient *fakeRemoteClient) Agent {
	t.Helper()

	return Agent{
		RemoteConfigLoader:  fakeRemoteConfigLoader{cfg: &rest.Config{Host: "https://remote.example.com"}}.Load,
		RemoteClientFactory: fakeRemoteClientFactory{client: remoteClient}.New,
		Now:                 func() time.Time { return now },
		Options: Options{
			Kubeconfig:    "/managed/config/kubeconfig",
			TokenFile:     filepath.Join(t.TempDir(), "token"),
			AWSConfigFile: filepath.Join(t.TempDir(), "aws-config"),
		},
	}
}

func workloadServiceAccount(annotations map[string]string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "app",
			Name:        "workload",
			Annotations: annotations,
		},
	}
}

type fakeRemoteConfigLoader struct {
	cfg *rest.Config
	err error
}

func (l fakeRemoteConfigLoader) Load(string) (*rest.Config, error) {
	return l.cfg, l.err
}

type fakeRemoteClientFactory struct {
	client *fakeRemoteClient
	err    error
}

func (f fakeRemoteClientFactory) New(*rest.Config) (RemoteClient, error) {
	return f.client, f.err
}

type fakeRemoteClient struct {
	username               string
	selfSubjectReviewErr   error
	selfSubjectReviewCalls int
	serviceAccounts        map[types.NamespacedName]*corev1.ServiceAccount
	getServiceAccountCalls []types.NamespacedName
	tokenResponse          TokenResponse
	tokenErr               error
	tokenResponses         []TokenResponse
	tokenErrs              []error
	tokenRequests          []tokenRequestCall
}

type tokenRequestCall struct {
	serviceAccount types.NamespacedName
	audience       string
	expiration     time.Duration
}

func (c *fakeRemoteClient) CurrentUsername(context.Context) (string, error) {
	c.selfSubjectReviewCalls++
	if c.selfSubjectReviewErr != nil {
		return "", c.selfSubjectReviewErr
	}

	return c.username, nil
}

func (c *fakeRemoteClient) GetServiceAccount(_ context.Context, serviceAccount types.NamespacedName) (*corev1.ServiceAccount, error) {
	c.getServiceAccountCalls = append(c.getServiceAccountCalls, serviceAccount)
	if c.serviceAccounts == nil {
		return nil, errors.New("not found")
	}

	sa := c.serviceAccounts[serviceAccount]
	if sa == nil {
		return nil, errors.New("not found")
	}

	return sa, nil
}

func (c *fakeRemoteClient) RequestServiceAccountToken(_ context.Context, serviceAccount types.NamespacedName, audience string, expiration time.Duration) (TokenResponse, error) {
	idx := len(c.tokenRequests)

	c.tokenRequests = append(c.tokenRequests, tokenRequestCall{
		serviceAccount: serviceAccount,
		audience:       audience,
		expiration:     expiration,
	})
	if idx < len(c.tokenErrs) && c.tokenErrs[idx] != nil {
		return TokenResponse{}, c.tokenErrs[idx]
	}

	if idx < len(c.tokenResponses) {
		return c.tokenResponses[idx], nil
	}

	if c.tokenErr != nil {
		return TokenResponse{}, c.tokenErr
	}

	return c.tokenResponse, nil
}

type recordingSleeper struct {
	durations []time.Duration
	err       error
	errs      []error
}

func (s *recordingSleeper) Sleep(_ context.Context, d time.Duration) error {
	s.durations = append(s.durations, d)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]

		return err
	}

	return s.err
}
