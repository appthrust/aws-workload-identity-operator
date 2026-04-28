package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestParseOptions(t *testing.T) {
	opts, err := parseOptions([]string{
		"--namespace", "wlc-a",
		"--service-account", "app/workload",
		"--aws-service-account-role", "role-a",
		"--clusterprofile-provider-file", "/tmp/providers.json",
		"--cluster-profile-namespaces", "awio-system,team-a,awio-system",
		"--region", "ap-northeast-1",
		"--token-expiration", "10m",
		"--session-duration", "1h",
		"--session-name", "app-workload",
		"--kubeconfig", "/tmp/kubeconfig",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseOptions returned error: %v", err)
	}

	if opts.Access.ProviderFile != "/tmp/providers.json" {
		t.Fatalf("providerFile = %q, want /tmp/providers.json", opts.Access.ProviderFile)
	}

	if want := []string{"awio-system", "team-a"}; !equalStringSlices(opts.ClusterProfileNamespaces, want) {
		t.Fatalf("ClusterProfileNamespaces = %#v, want %#v", opts.ClusterProfileNamespaces, want)
	}

	if opts.WorkloadNamespace != "wlc-a" {
		t.Fatalf("WorkloadNamespace = %q, want wlc-a", opts.WorkloadNamespace)
	}

	if opts.ServiceAccount != (types.NamespacedName{Namespace: "app", Name: "workload"}) {
		t.Fatalf("ServiceAccount = %#v", opts.ServiceAccount)
	}

	if opts.AWSServiceAccountRoleName != "role-a" {
		t.Fatalf("AWSServiceAccountRoleName = %q", opts.AWSServiceAccountRoleName)
	}

	if opts.Region != "ap-northeast-1" {
		t.Fatalf("Region = %q", opts.Region)
	}

	if opts.TokenExpiration != 10*time.Minute {
		t.Fatalf("TokenExpiration = %s", opts.TokenExpiration)
	}

	if opts.SessionDuration != time.Hour {
		t.Fatalf("SessionDuration = %s", opts.SessionDuration)
	}

	if opts.SessionName != "app-workload" {
		t.Fatalf("SessionName = %q", opts.SessionName)
	}

	if opts.Kubeconfig != "/tmp/kubeconfig" {
		t.Fatalf("Kubeconfig = %q", opts.Kubeconfig)
	}
}

func TestParseOptionsAllowsClusterProfileWildcard(t *testing.T) {
	opts, err := parseOptions([]string{
		"--namespace", "wlc-a",
		"--service-account", "app/workload",
		"--clusterprofile-provider-file", "/tmp/providers.json",
		"--cluster-profile-namespaces", "*",
		"--session-name", "app-workload",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseOptions returned error: %v", err)
	}

	if want := []string{"*"}; !equalStringSlices(opts.ClusterProfileNamespaces, want) {
		t.Fatalf("ClusterProfileNamespaces = %#v, want %#v", opts.ClusterProfileNamespaces, want)
	}
}

func TestParseOptionsRejectsMixedClusterProfileWildcard(t *testing.T) {
	_, err := parseOptions([]string{
		"--namespace", "wlc-a",
		"--service-account", "app/workload",
		"--clusterprofile-provider-file", "/tmp/providers.json",
		"--cluster-profile-namespaces", "awio-system,*",
		"--session-name", "app-workload",
	}, io.Discard)
	if err == nil {
		t.Fatal("parseOptions returned nil error")
	}

	if err.Error() != "--cluster-profile-namespaces must be either \"*\" or explicit namespaces, not both" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestParseOptionsRequiresFields(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "namespace",
			args: []string{
				"--service-account", "app/workload",
				"--clusterprofile-provider-file", "/tmp/providers.json",
				"--session-name", "app-workload",
			},
			want: "--namespace is required",
		},
		{
			name: "service account",
			args: []string{
				"--namespace", "wlc-a",
				"--clusterprofile-provider-file", "/tmp/providers.json",
				"--session-name", "app-workload",
			},
			want: "--service-account must be namespace/name",
		},
		{
			name: "provider file",
			args: []string{
				"--namespace", "wlc-a",
				"--service-account", "app/workload",
				"--cluster-profile-namespaces", "awio-system",
				"--session-name", "app-workload",
			},
			want: "--clusterprofile-provider-file is required",
		},
		{
			name: "cluster profile namespaces",
			args: []string{
				"--namespace", "wlc-a",
				"--service-account", "app/workload",
				"--clusterprofile-provider-file", "/tmp/providers.json",
				"--session-name", "app-workload",
			},
			want: "--cluster-profile-namespaces is required",
		},
		{
			name: "session name",
			args: []string{
				"--namespace", "wlc-a",
				"--service-account", "app/workload",
				"--clusterprofile-provider-file", "/tmp/providers.json",
				"--cluster-profile-namespaces", "awio-system",
			},
			want: "--session-name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseOptions(tt.args, io.Discard)
			if err == nil {
				t.Fatal("parseOptions returned nil error")
			}

			if err.Error() != tt.want {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestParseOptionsRejectsTooShortTokenExpiration(t *testing.T) {
	_, err := parseOptions([]string{
		"--namespace", "wlc-a",
		"--service-account", "app/workload",
		"--clusterprofile-provider-file", "/tmp/providers.json",
		"--cluster-profile-namespaces", "awio-system",
		"--session-name", "app-workload",
		"--token-expiration", "9m59s",
	}, io.Discard)
	if err == nil {
		t.Fatal("parseOptions returned nil error")
	}

	if err.Error() != "--token-expiration must be at least 10m" {
		t.Fatalf("error = %q, want %q", err.Error(), "--token-expiration must be at least 10m")
	}
}

func TestParseOptionsHelpFlagWritesUsage(t *testing.T) {
	for _, flagName := range []string{"--help", "-h"} {
		t.Run(flagName, func(t *testing.T) {
			var out bytes.Buffer

			_, err := parseOptions([]string{flagName}, &out)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("parseOptions error = %v, want flag.ErrHelp", err)
			}

			usage := out.String()
			if !strings.Contains(usage, "-service-account") {
				t.Fatalf("usage missing -service-account flag: %q", usage)
			}

			if !strings.Contains(usage, "-clusterprofile-provider-file") {
				t.Fatalf("usage missing -clusterprofile-provider-file flag: %q", usage)
			}
		})
	}
}
