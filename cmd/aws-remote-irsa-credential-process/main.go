// Package main implements an AWS credential_process helper for remote IRSA.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, productionDependencies()))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) int {
	opts, err := parseOptions(args)
	if err != nil {
		writeError(stderr, err)

		return 1
	}

	creds, err := retrieveCredentials(ctx, &opts, deps)
	if err != nil {
		writeError(stderr, err)

		return 1
	}

	if err := writeCredentialProcessOutput(stdout, &creds); err != nil {
		writeError(stderr, fmt.Errorf("write credential_process output: %w", err))

		return 1
	}

	return 0
}

func writeError(stderr io.Writer, err error) {
	_, _ = fmt.Fprintf(stderr, "%s\n", sanitizeError(err))
}
