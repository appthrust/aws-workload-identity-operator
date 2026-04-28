// Package main implements an AWS credential_process helper for remote IRSA.
package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/appthrust/aws-workload-identity-operator/internal/remoteirsacredentialprocess"
)

func main() {
	opts, err := parseOptions(os.Args[1:], os.Stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}

		remoteirsacredentialprocess.WriteError(os.Stderr, err)
		os.Exit(1)
	}

	os.Exit(remoteirsacredentialprocess.Run(
		context.Background(),
		&opts,
		os.Stdout,
		os.Stderr,
		remoteirsacredentialprocess.ProductionDependencies(),
	))
}
