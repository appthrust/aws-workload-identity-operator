// Package tokenfile refreshes remote IRSA token and AWS config files.
package tokenfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	serviceaccount "k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

const (
	tokenExpiration    = 10 * time.Minute
	refreshBefore      = 9 * time.Minute
	refreshRetryDelay  = 30 * time.Second
	roleSessionPrefix  = "awio"
	roleSessionHashLen = 12
	roleSessionMaxLen  = 64
)

// Options configures Agent.
type Options struct {
	Kubeconfig    string
	TokenFile     string
	AWSConfigFile string
}

// TokenResponse is the result of a remote ServiceAccount TokenRequest.
type TokenResponse struct {
	Token               string
	ExpirationTimestamp time.Time
}

// RemoteClient performs the remote Kubernetes API calls needed by the sidecar.
type RemoteClient interface {
	CurrentUsername(ctx context.Context) (string, error)
	GetServiceAccount(ctx context.Context, serviceAccount types.NamespacedName) (*corev1.ServiceAccount, error)
	RequestServiceAccountToken(ctx context.Context, serviceAccount types.NamespacedName, audience string, expiration time.Duration) (TokenResponse, error)
}

// RemoteConfigLoader loads a rest.Config from a kubeconfig path.
type RemoteConfigLoader func(path string) (*rest.Config, error)

// RemoteClientFactory creates a RemoteClient for a rest.Config.
type RemoteClientFactory func(config *rest.Config) (RemoteClient, error)

// Agent refreshes a remote ServiceAccount token file and AWS shared config for
// hosted add-on workloads.
type Agent struct {
	RemoteConfigLoader  RemoteConfigLoader
	RemoteClientFactory RemoteClientFactory
	Now                 func() time.Time
	Options             Options
}

// Sleeper waits before the next token refresh.
type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

// RealSleeper waits with a timer and returns early when the context is done.
type RealSleeper struct{}

// Sleep waits for d or until ctx is canceled.
func (RealSleeper) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("sleep interrupted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// runState caches the per-process remote resources across Refresh invocations
// so the long-running sidecar avoids re-reading the kubeconfig, rebuilding the
// HTTP client, and re-running SelfSubjectReview on every refresh tick.
type runState struct {
	remoteConfig      *rest.Config
	remoteClient      RemoteClient
	serviceAccountRef types.NamespacedName
	serviceAccountSet bool
}

// Refresh resolves the remote ServiceAccount, requests a TokenRequest token,
// writes the token and AWS config atomically, and returns the next refresh delay.
func (a Agent) Refresh(ctx context.Context) (time.Duration, error) {
	if err := a.validate(); err != nil {
		return 0, err
	}

	var state runState

	return a.refreshOnce(ctx, &state)
}

// Run refreshes once immediately, then refreshes on the returned schedule. A
// later refresh failure keeps existing files and retries after a short delay.
func (a Agent) Run(ctx context.Context, sleeper Sleeper) error {
	if sleeper == nil {
		sleeper = RealSleeper{}
	}

	if err := a.validate(); err != nil {
		return err
	}

	var state runState
	for {
		wait, err := a.refreshOnce(ctx, &state)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			wait = refreshRetryDelay
		}

		if err := sleeper.Sleep(ctx, wait); err != nil {
			return fmt.Errorf("sleep before next refresh: %w", err)
		}
	}
}

func (a Agent) refreshOnce(ctx context.Context, state *runState) (time.Duration, error) {
	if err := a.ensureRunState(ctx, state); err != nil {
		return 0, err
	}

	serviceAccount, err := state.remoteClient.GetServiceAccount(ctx, state.serviceAccountRef)
	if err != nil {
		return 0, fmt.Errorf("get remote ServiceAccount %s: %w", state.serviceAccountRef, err)
	}

	roleARN := serviceAccount.Annotations[remoteirsa.ServiceAccountRoleARNAnnotation]
	if roleARN == "" {
		return 0, fmt.Errorf("remote ServiceAccount %s missing %s annotation", state.serviceAccountRef, remoteirsa.ServiceAccountRoleARNAnnotation)
	}

	token, err := state.remoteClient.RequestServiceAccountToken(ctx, state.serviceAccountRef, remoteirsa.STSAudience, tokenExpiration)
	if err != nil {
		return 0, fmt.Errorf("request remote ServiceAccount token: %w", err)
	}

	return a.writeRefreshFiles(state, roleARN, token)
}

func (a Agent) ensureRunState(ctx context.Context, state *runState) error {
	if state.remoteConfig == nil {
		cfg, err := a.remoteConfigLoader()(a.Options.Kubeconfig)
		if err != nil {
			return fmt.Errorf("build remote kubeconfig: %w", err)
		}

		state.remoteConfig = cfg
	}

	if state.remoteClient == nil {
		cli, err := a.remoteClientFactory()(state.remoteConfig)
		if err != nil {
			return fmt.Errorf("create remote Kubernetes client: %w", err)
		}

		state.remoteClient = cli
	}

	if !state.serviceAccountSet {
		ref, err := a.resolveServiceAccount(ctx, state.remoteClient)
		if err != nil {
			return err
		}

		state.serviceAccountRef = ref
		state.serviceAccountSet = true
	}

	return nil
}

func (a Agent) writeRefreshFiles(state *runState, roleARN string, token TokenResponse) (time.Duration, error) {
	if token.ExpirationTimestamp.IsZero() {
		return 0, fmt.Errorf("remote ServiceAccount TokenRequest did not return expirationTimestamp")
	}

	if err := WriteTokenAtomic(a.Options.TokenFile, token.Token); err != nil {
		return 0, err
	}

	if err := WriteAWSConfigAtomic(a.Options.AWSConfigFile, AWSConfig{
		RoleARN:              roleARN,
		WebIdentityTokenFile: a.Options.TokenFile,
		RoleSessionName:      roleSessionName(state.remoteConfig, state.serviceAccountRef),
	}); err != nil {
		return 0, err
	}

	wait := token.ExpirationTimestamp.Sub(a.now()) - refreshBefore
	if wait <= 0 {
		return refreshRetryDelay, nil
	}

	return wait, nil
}

func (a Agent) resolveServiceAccount(ctx context.Context, remoteClient RemoteClient) (types.NamespacedName, error) {
	username, err := remoteClient.CurrentUsername(ctx)
	if err != nil {
		return types.NamespacedName{}, fmt.Errorf("create remote SelfSubjectReview: %w", err)
	}

	namespace, name, err := serviceaccount.SplitUsername(username)
	if err != nil {
		return types.NamespacedName{}, fmt.Errorf("cannot infer ServiceAccount from SelfSubjectReview username %q: %w", username, err)
	}

	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}

func (a Agent) validate() error {
	switch {
	case a.Options.Kubeconfig == "":
		return fmt.Errorf("kubeconfig is required")
	case a.Options.TokenFile == "":
		return fmt.Errorf("token file is required")
	case a.Options.AWSConfigFile == "":
		return fmt.Errorf("aws config file is required")
	default:
		return nil
	}
}

func roleSessionName(config *rest.Config, serviceAccount types.NamespacedName) string {
	host := ""
	if config != nil {
		host = config.Host
	}

	sum := sha256.Sum256([]byte(host + "\x00" + serviceAccount.Namespace + "\x00" + serviceAccount.Name))
	hash := hex.EncodeToString(sum[:])[:roleSessionHashLen]

	base := sanitizeRoleSessionNamePart(serviceAccount.Namespace + "-" + serviceAccount.Name)
	if base == "" {
		base = "serviceaccount"
	}

	maxBaseLen := roleSessionMaxLen - len(roleSessionPrefix) - 1 - 1 - roleSessionHashLen
	if len(base) > maxBaseLen {
		base = strings.Trim(base[:maxBaseLen], "-")
	}

	if base == "" {
		base = "sa"
	}

	return fmt.Sprintf("%s-%s-%s", roleSessionPrefix, base, hash)
}

func sanitizeRoleSessionNamePart(value string) string {
	var b strings.Builder

	lastDash := false

	for _, r := range value {
		if isRoleSessionNameRune(r) {
			b.WriteRune(r)

			lastDash = false

			continue
		}

		if !lastDash {
			b.WriteByte('-')

			lastDash = true
		}
	}

	return strings.Trim(b.String(), "-")
}

func isRoleSessionNameRune(r rune) bool {
	return (r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') ||
		r == '_' ||
		r == '+' ||
		r == '=' ||
		r == ',' ||
		r == '.' ||
		r == '@' ||
		r == '-'
}

func (a Agent) remoteConfigLoader() RemoteConfigLoader {
	if a.RemoteConfigLoader != nil {
		return a.RemoteConfigLoader
	}

	return func(path string) (*rest.Config, error) {
		return clientcmd.BuildConfigFromFlags("", path)
	}
}

func (a Agent) remoteClientFactory() RemoteClientFactory {
	if a.RemoteClientFactory != nil {
		return a.RemoteClientFactory
	}

	return NewRemoteClient
}

func (a Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}

	return time.Now()
}

// NewRemoteClient returns the production remote Kubernetes client.
func NewRemoteClient(config *rest.Config) (RemoteClient, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}

	return productionRemoteClient{clientset: clientset}, nil
}

type productionRemoteClient struct {
	clientset kubernetes.Interface
}

func (c productionRemoteClient) CurrentUsername(ctx context.Context) (string, error) {
	review, err := c.clientset.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create SelfSubjectReview: %w", err)
	}

	return review.Status.UserInfo.Username, nil
}

func (c productionRemoteClient) GetServiceAccount(ctx context.Context, serviceAccount types.NamespacedName) (*corev1.ServiceAccount, error) {
	account, err := c.clientset.CoreV1().ServiceAccounts(serviceAccount.Namespace).Get(ctx, serviceAccount.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get ServiceAccount %s: %w", serviceAccount, err)
	}

	return account, nil
}

func (c productionRemoteClient) RequestServiceAccountToken(ctx context.Context, serviceAccount types.NamespacedName, audience string, expiration time.Duration) (TokenResponse, error) {
	expirationSeconds := int64(expiration / time.Second)
	request := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expirationSeconds,
		},
	}

	response, err := c.clientset.CoreV1().
		ServiceAccounts(serviceAccount.Namespace).
		CreateToken(ctx, serviceAccount.Name, request, metav1.CreateOptions{})
	if err != nil {
		return TokenResponse{}, fmt.Errorf("create ServiceAccount token for %s: %w", serviceAccount, err)
	}

	return TokenResponse{
		Token:               response.Status.Token,
		ExpirationTimestamp: response.Status.ExpirationTimestamp.Time,
	}, nil
}
