package remoteirsacredentialprocess

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

const (
	// MinimumTokenExpiration is the minimum Kubernetes TokenRequest lifetime
	// accepted by the credential process.
	MinimumTokenExpiration = 10 * time.Minute
)

// CredentialProvider retrieves AWS credentials for the resolved workload role.
type CredentialProvider interface {
	Retrieve(context.Context) (awssdk.Credentials, error)
}

// Dependencies contains injectable collaborators used by the runtime.
type Dependencies struct {
	BuildAccessConfig func(AccessConfigOptions) (*access.Config, error)
	BuildHubConfig    func(string) (*rest.Config, error)
	NewHubClient      func(*rest.Config) (client.Client, error)
	NewHubKubeClient  func(*rest.Config) (kubernetes.Interface, error)
	NewProvider       func(remoteirsa.Options) (CredentialProvider, error)
}

// Options configures a credential_process invocation.
type Options struct {
	Access                    AccessConfigOptions
	Kubeconfig                string
	WorkloadNamespace         string
	ClusterProfileNamespaces  []string
	AWSServiceAccountRoleName string
	ServiceAccount            types.NamespacedName
	Region                    string
	TokenExpiration           time.Duration
	SessionDuration           time.Duration
	SessionName               string
}

// CredentialProcessOutput is the JSON shape emitted to stdout for AWS SDKs.
//
//nolint:tagliatelle // AWS credential_process requires these exact JSON field names.
type CredentialProcessOutput struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration,omitempty"`
}

// ProductionDependencies returns dependencies backed by production clients.
func ProductionDependencies() Dependencies {
	return Dependencies{
		BuildAccessConfig: BuildAccessConfig,
		BuildHubConfig:    BuildHubConfig,
		NewHubClient:      NewHubClient,
		NewHubKubeClient:  NewHubKubeClient,
		NewProvider: func(opts remoteirsa.Options) (CredentialProvider, error) {
			return remoteirsa.NewProvider(opts)
		},
	}
}

// Run executes the credential_process flow and returns a process exit code.
func Run(ctx context.Context, opts *Options, stdout, stderr io.Writer, deps Dependencies) int {
	creds, err := RetrieveCredentials(ctx, opts, deps)
	if err != nil {
		WriteError(stderr, err)

		return 1
	}

	if err := WriteCredentialProcessOutput(stdout, &creds); err != nil {
		WriteError(stderr, fmt.Errorf("write credential_process output: %w", err))

		return 1
	}

	return 0
}

// RetrieveCredentials resolves remote IRSA inputs and retrieves AWS credentials.
func RetrieveCredentials(ctx context.Context, opts *Options, deps Dependencies) (awssdk.Credentials, error) {
	if err := validateDependencies(deps); err != nil {
		return awssdk.Credentials{}, err
	}

	accessConfig, err := deps.BuildAccessConfig(opts.Access)
	if err != nil {
		return awssdk.Credentials{}, err
	}

	hubConfig, err := deps.BuildHubConfig(opts.Kubeconfig)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("build hub kubeconfig: %w", err)
	}

	hubClient, err := deps.NewHubClient(hubConfig)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("create hub API client: %w", err)
	}

	hubKubeClient, err := deps.NewHubKubeClient(hubConfig)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("create hub Kubernetes client: %w", err)
	}

	providerOpts := remoteirsa.Options{
		HubReader:                  hubClient,
		HubKubeClient:              hubKubeClient,
		ClusterProfileAccessConfig: accessConfig,
		ClusterProfileNamespaces:   opts.ClusterProfileNamespaces,
		WorkloadNamespace:          opts.WorkloadNamespace,
		AWSServiceAccountRoleName:  opts.AWSServiceAccountRoleName,
		ServiceAccount:             opts.ServiceAccount,
		Region:                     opts.Region,
		TokenExpiration:            opts.TokenExpiration,
		SessionDuration:            opts.SessionDuration,
		SessionName:                opts.SessionName,
	}

	provider, err := deps.NewProvider(providerOpts)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("create remote IRSA provider: %w", err)
	}

	credentials, err := provider.Retrieve(ctx)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("retrieve remote IRSA credentials: %w", err)
	}

	return credentials, nil
}

func validateDependencies(deps Dependencies) error {
	switch {
	case deps.BuildAccessConfig == nil:
		return fmt.Errorf("BuildAccessConfig dependency is required")
	case deps.BuildHubConfig == nil:
		return fmt.Errorf("BuildHubConfig dependency is required")
	case deps.NewHubClient == nil:
		return fmt.Errorf("NewHubClient dependency is required")
	case deps.NewHubKubeClient == nil:
		return fmt.Errorf("NewHubKubeClient dependency is required")
	case deps.NewProvider == nil:
		return fmt.Errorf("NewProvider dependency is required")
	default:
		return nil
	}
}

// WriteCredentialProcessOutput writes AWS credential_process JSON to w.
func WriteCredentialProcessOutput(w io.Writer, creds *awssdk.Credentials) error {
	output := CredentialProcessOutput{
		Version:         1,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
	}

	if creds.CanExpire && !creds.Expires.IsZero() {
		output.Expiration = creds.Expires.UTC().Format(time.RFC3339)
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(output); err != nil { //nolint:gosec // credential_process intentionally writes session credentials to stdout.
		return fmt.Errorf("encode credential_process output: %w", err)
	}

	return nil
}

// WriteError writes a sanitized credential_process error to stderr.
func WriteError(stderr io.Writer, err error) {
	_, _ = fmt.Fprintf(stderr, "%s\n", SanitizeError(err))
}

// HandleFlagParseError reduces a *flag.FlagSet.Parse result to the value the
// CLI binaries actually want to return. If err is flag.ErrHelp, usage is
// printed to helpOut and flag.ErrHelp is returned unchanged so the caller can
// translate it into a success exit. Any other non-nil err is wrapped as
// "parse flags: %w" so the suite of CLIs surfaces parse failures the same
// way. A nil err returns nil.
func HandleFlagParseError(fs *flag.FlagSet, err error, helpOut io.Writer) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(helpOut)
		fs.Usage()

		return flag.ErrHelp
	}

	return fmt.Errorf("parse flags: %w", err)
}

// secretMarkers is the alnum-normalized set of substrings whose presence in an
// error message indicates the message likely embeds credential material; the
// message is replaced wholesale with a generic failure string in that case.
var secretMarkers = []string{
	"accesskeyid",
	"secretaccesskey",
	"sessiontoken",
	"bearertoken",
	"webidentitytoken",
	"authorizationbearer",
}

const sanitizedCredentialProcessFailure = "remote IRSA credential_process failed" //nolint:gosec // Generic failure message, not credential material.

// SanitizeError redacts errors that appear to contain credential material.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}

	message := err.Error()
	normalized := normalizeSecretMarker(message)

	for _, marker := range secretMarkers {
		if strings.Contains(normalized, marker) {
			return sanitizedCredentialProcessFailure
		}
	}

	return message
}

func normalizeSecretMarker(s string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}

	return b.String()
}

// BuildHubConfig loads a hub Kubernetes REST config.
func BuildHubConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig from %q: %w", kubeconfig, err)
		}

		return config, nil
	}

	inClusterConfig, err := rest.InClusterConfig()
	if err == nil {
		return inClusterConfig, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, loadingErr := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if loadingErr != nil {
		return nil, fmt.Errorf("in-cluster config unavailable and default kubeconfig failed: %w", loadingErr)
	}

	return config, nil
}

// NewHubClient returns a controller-runtime hub client.
func NewHubClient(config *rest.Config) (client.Client, error) {
	c, err := client.New(config, client.Options{Scheme: HubScheme()})
	if err != nil {
		return nil, fmt.Errorf("create controller-runtime client: %w", err)
	}

	return c, nil
}

// NewHubKubeClient returns a typed Kubernetes hub client.
func NewHubKubeClient(config *rest.Config) (kubernetes.Interface, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes clientset: %w", err)
	}

	return clientset, nil
}

// HubScheme returns the runtime scheme used for hub API reads.
func HubScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(identityv1.AddToScheme(scheme))
	utilruntime.Must(clusterinventoryv1alpha1.AddToScheme(scheme))

	return scheme
}
