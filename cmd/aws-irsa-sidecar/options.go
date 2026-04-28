package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa/tokenfile"
)

type options struct {
	Agent tokenfile.Options
}

func parseOptions(args []string) (options, error) {
	opts := options{}

	fs := flag.NewFlagSet("aws-irsa-sidecar", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&opts.Agent.Kubeconfig, "kubeconfig", "", "Required managed cluster kubeconfig path.")
	fs.StringVar(&opts.Agent.TokenFile, "token-file", "", "Path to write the remote ServiceAccount token.")
	fs.StringVar(&opts.Agent.AWSConfigFile, "aws-config-file", "", "Path to write the AWS shared config file.")

	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}

	if opts.Agent.Kubeconfig == "" {
		return options{}, fmt.Errorf("--kubeconfig is required")
	}
	if opts.Agent.TokenFile == "" {
		return options{}, fmt.Errorf("--token-file is required")
	}
	if opts.Agent.AWSConfigFile == "" {
		return options{}, fmt.Errorf("--aws-config-file is required")
	}

	return opts, nil
}
