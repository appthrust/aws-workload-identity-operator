// Package main implements an AWS credential_process helper for remote IRSA.
package main

import (
	"context"
	"os"

	"github.com/appthrust/aws-workload-identity-operator/internal/remoteirsacredentialprocess"
)

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
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
