package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/appthrust/aws-workload-identity-operator/internal/remoteirsacredentialprocess"
)

const defaultTokenExpiration = 15 * time.Minute

// parseOptions parses credential_process CLI flags. If -h/--help is requested
// it returns flag.ErrHelp after writing usage to helpOut, so the caller exits
// with code 0.
func parseOptions(args []string, helpOut io.Writer) (remoteirsacredentialprocess.Options, error) {
	opts := remoteirsacredentialprocess.Options{}

	fs := flag.NewFlagSet("aws-remote-irsa-credential-process", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	serviceAccount := fs.String("service-account", "", "Remote ServiceAccount as namespace/name.")
	fs.StringVar(&opts.WorkloadNamespace, "namespace", "", "Namespace containing AWSWorkloadIdentityConfig/default and AWSServiceAccountRole objects.")
	fs.StringVar(&opts.AWSServiceAccountRoleName, "aws-service-account-role", "", "Optional AWSServiceAccountRole name in --namespace.")
	fs.StringVar(&opts.Access.ProviderFile, "clusterprofile-provider-file", "", "Path to the ClusterProfile access provider config file.")
	fs.StringVar(&opts.Region, "region", "", "Optional AWS region override.")
	fs.DurationVar(&opts.TokenExpiration, "token-expiration", defaultTokenExpiration, "Remote ServiceAccount token expiration; Kubernetes requires at least 10m.")
	fs.DurationVar(&opts.SessionDuration, "session-duration", 0, "Optional STS session duration.")
	fs.StringVar(&opts.SessionName, "session-name", "", "STS role session name.")
	fs.StringVar(&opts.Kubeconfig, "kubeconfig", "", "Optional hub kubeconfig path.")

	if err := remoteirsacredentialprocess.HandleFlagParseError(fs, fs.Parse(args), helpOut); err != nil {
		return remoteirsacredentialprocess.Options{}, err
	}

	if fs.NArg() > 0 {
		return remoteirsacredentialprocess.Options{}, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}

	parsedServiceAccount, err := parseNamespacedName(*serviceAccount, "--service-account")
	if err != nil {
		return remoteirsacredentialprocess.Options{}, err
	}

	opts.ServiceAccount = parsedServiceAccount

	if err := validateParsedOptions(&opts); err != nil {
		return remoteirsacredentialprocess.Options{}, err
	}

	return opts, nil
}

func validateParsedOptions(opts *remoteirsacredentialprocess.Options) error {
	if opts.WorkloadNamespace == "" {
		return fmt.Errorf("--namespace is required")
	}

	if opts.Access.ProviderFile == "" {
		return fmt.Errorf("--clusterprofile-provider-file is required")
	}

	if opts.SessionName == "" {
		return fmt.Errorf("--session-name is required")
	}

	if opts.TokenExpiration <= 0 {
		return fmt.Errorf("--token-expiration must be greater than zero")
	}

	if opts.TokenExpiration < remoteirsacredentialprocess.MinimumTokenExpiration {
		return fmt.Errorf("--token-expiration must be at least 10m")
	}

	if opts.SessionDuration < 0 {
		return fmt.Errorf("--session-duration must be greater than or equal to zero")
	}

	return nil
}

func parseNamespacedName(raw, flagName string) (types.NamespacedName, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return types.NamespacedName{}, fmt.Errorf("%s must be namespace/name", flagName)
	}

	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}
