package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOptions(t *testing.T) {
	opts, err := parseOptions([]string{
		"--kubeconfig", "/managed/config/kubeconfig",
		"--token-file", "/var/run/aws-irsa/token",
		"--aws-config-file", "/var/run/aws-irsa/config",
	})
	if err != nil {
		t.Fatalf("parseOptions returned error: %v", err)
	}

	if opts.Agent.Kubeconfig != "/managed/config/kubeconfig" {
		t.Fatalf("Kubeconfig = %q, want /managed/config/kubeconfig", opts.Agent.Kubeconfig)
	}

	if opts.Agent.TokenFile != "/var/run/aws-irsa/token" {
		t.Fatalf("TokenFile = %q", opts.Agent.TokenFile)
	}

	if opts.Agent.AWSConfigFile != "/var/run/aws-irsa/config" {
		t.Fatalf("AWSConfigFile = %q", opts.Agent.AWSConfigFile)
	}
}

func TestParseOptionsRequiresKubeconfig(t *testing.T) {
	_, err := parseOptions([]string{
		"--token-file", "/tmp/token",
		"--aws-config-file", "/tmp/aws-config",
	})
	if err == nil {
		t.Fatal("parseOptions returned nil error")
	}

	if err.Error() != "--kubeconfig is required" {
		t.Fatalf("error = %q, want %q", err.Error(), "--kubeconfig is required")
	}
}

func TestParseOptionsRejectsCustomizationFlags(t *testing.T) {
	tests := []string{
		"--aws-profile",
		"--token-file-mode",
		"--role-session-name",
		"--aws-region",
		"--token-expiration",
		"--refresh-before",
		"--service-account-namespace",
		"--service-account-name",
		"--namespace",
		"--service-account",
		"--aws-service-account-role",
		"--clusterprofile-provider-file",
		"--remote-kubeconfig",
	}

	for _, flagName := range tests {
		t.Run(flagName, func(t *testing.T) {
			_, err := parseOptions([]string{
				flagName, "value",
				"--kubeconfig", "/managed/config/kubeconfig",
				"--token-file", "/tmp/token",
				"--aws-config-file", "/tmp/aws-config",
			})
			if err == nil {
				t.Fatal("parseOptions returned nil error")
			}

			want := "flag provided but not defined: -" + strings.TrimPrefix(flagName, "--")
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want unknown flag containing %q", err.Error(), want)
			}
		})
	}
}

func TestRunCheckRequiresNonEmptyTokenFileAndOptionalAWSConfigFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	awsConfigFile := filepath.Join(dir, "aws-config")

	if got := runCheck([]string{"--token-file", tokenFile, "--aws-config-file", awsConfigFile}); got != 1 {
		t.Fatalf("runCheck missing files = %d, want 1", got)
	}

	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	if got := runCheck([]string{"--token-file", tokenFile}); got != 0 {
		t.Fatalf("runCheck token-only compatibility = %d, want 0", got)
	}

	if got := runCheck([]string{"--token-file", tokenFile, "--aws-config-file", awsConfigFile}); got != 1 {
		t.Fatalf("runCheck missing AWS config = %d, want 1", got)
	}

	if err := os.WriteFile(awsConfigFile, []byte("[default]\n"), 0o600); err != nil {
		t.Fatalf("write AWS config file: %v", err)
	}

	if got := runCheck([]string{"--token-file", tokenFile, "--aws-config-file", awsConfigFile}); got != 0 {
		t.Fatalf("runCheck both files = %d, want 0", got)
	}
}

func TestRunCheckRejectsUnexpectedPositionalArgument(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	if got := runCheck([]string{"--token-file", tokenFile, "extra"}); got != 1 {
		t.Fatalf("runCheck positional argument = %d, want 1", got)
	}
}
