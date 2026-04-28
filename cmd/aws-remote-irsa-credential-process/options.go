package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const defaultTokenExpiration = 15 * time.Minute

type options struct {
	namespace                  string
	serviceAccount             types.NamespacedName
	awsServiceAccountRoleName  string
	clusterProfileProviderFile string
	region                     string
	tokenExpiration            time.Duration
	sessionDuration            time.Duration
	sessionName                string
	kubeconfig                 string
}

func parseOptions(args []string) (options, error) {
	opts := options{}

	fs := flag.NewFlagSet("aws-remote-irsa-credential-process", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.namespace, "namespace", "", "Namespace containing AWSWorkloadIdentityConfig/default and AWSServiceAccountRole objects.")
	serviceAccount := fs.String("service-account", "", "Remote ServiceAccount as namespace/name.")
	fs.StringVar(&opts.awsServiceAccountRoleName, "aws-service-account-role", "", "Optional AWSServiceAccountRole name in --namespace.")
	fs.StringVar(&opts.clusterProfileProviderFile, "clusterprofile-provider-file", "", "Path to the ClusterProfile access provider config file.")
	fs.StringVar(&opts.region, "region", "", "Optional AWS region override.")
	fs.DurationVar(&opts.tokenExpiration, "token-expiration", defaultTokenExpiration, "Remote ServiceAccount token expiration.")
	fs.DurationVar(&opts.sessionDuration, "session-duration", 0, "Optional STS session duration.")
	fs.StringVar(&opts.sessionName, "session-name", "", "STS role session name.")
	fs.StringVar(&opts.kubeconfig, "kubeconfig", "", "Optional hub kubeconfig path.")

	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}

	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}

	parsedServiceAccount, err := parseNamespacedName(*serviceAccount, "--service-account")
	if err != nil {
		return options{}, err
	}

	opts.serviceAccount = parsedServiceAccount

	if opts.namespace == "" {
		return options{}, fmt.Errorf("--namespace is required")
	}

	if opts.clusterProfileProviderFile == "" {
		return options{}, fmt.Errorf("--clusterprofile-provider-file is required")
	}

	if opts.sessionName == "" {
		return options{}, fmt.Errorf("--session-name is required")
	}

	if opts.tokenExpiration <= 0 {
		return options{}, fmt.Errorf("--token-expiration must be greater than zero")
	}

	if opts.sessionDuration < 0 {
		return options{}, fmt.Errorf("--session-duration must be greater than or equal to zero")
	}

	return opts, nil
}

func parseNamespacedName(raw, flagName string) (types.NamespacedName, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return types.NamespacedName{}, fmt.Errorf("%s must be namespace/name", flagName)
	}

	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}
