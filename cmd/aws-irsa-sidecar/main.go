// Package main implements the AWS IRSA sidecar.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/appthrust/aws-workload-identity-operator/internal/remoteirsacredentialprocess"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa/tokenfile"
)

func main() {
	os.Exit(run(ctrl.SetupSignalHandler(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) > 0 && args[0] == "check" {
		return runCheck(args[1:], os.Stdout)
	}

	opts, err := parseOptions(args, os.Stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}

		remoteirsacredentialprocess.WriteError(os.Stderr, err)

		return 1
	}

	agent := tokenfile.Agent{
		Options: opts.Agent,
	}

	if err := agent.Run(ctx, nil); err != nil {
		remoteirsacredentialprocess.WriteError(os.Stderr, err)

		return 1
	}

	return 0
}

func runCheck(args []string, helpOut io.Writer) int {
	fs := flag.NewFlagSet("aws-irsa-sidecar check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	tokenFile := fs.String("token-file", "", "Path to the remote ServiceAccount token file.")

	awsConfigFile := fs.String("aws-config-file", "", "Path to the generated AWS shared config file.")

	if err := remoteirsacredentialprocess.HandleFlagParseError(fs, fs.Parse(args), helpOut); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}

		remoteirsacredentialprocess.WriteError(os.Stderr, err)

		return 1
	}

	if fs.NArg() > 0 {
		remoteirsacredentialprocess.WriteError(os.Stderr, fmt.Errorf("unexpected positional argument %q", fs.Arg(0)))

		return 1
	}

	if *tokenFile == "" {
		remoteirsacredentialprocess.WriteError(os.Stderr, fmt.Errorf("--token-file is required"))

		return 1
	}

	if err := checkNonEmptyRegularFile("--token-file", *tokenFile); err != nil {
		remoteirsacredentialprocess.WriteError(os.Stderr, err)

		return 1
	}

	if *awsConfigFile == "" {
		remoteirsacredentialprocess.WriteError(os.Stderr, fmt.Errorf("--aws-config-file is required"))

		return 1
	}

	if err := checkNonEmptyRegularFile("--aws-config-file", *awsConfigFile); err != nil {
		remoteirsacredentialprocess.WriteError(os.Stderr, err)

		return 1
	}

	return 0
}

func checkNonEmptyRegularFile(flag, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s %q: %w", flag, path, err)
	}

	if info.IsDir() {
		return fmt.Errorf("%s %q is a directory", flag, path)
	}

	if info.Size() == 0 {
		return fmt.Errorf("%s %q is empty", flag, path)
	}

	return nil
}
