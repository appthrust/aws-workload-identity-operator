package main

import (
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
		"--region", "ap-northeast-1",
		"--token-expiration", "10m",
		"--session-duration", "1h",
		"--session-name", "app-workload",
		"--kubeconfig", "/tmp/kubeconfig",
	})
	if err != nil {
		t.Fatalf("parseOptions returned error: %v", err)
	}

	if opts.namespace != "wlc-a" {
		t.Fatalf("namespace = %q, want wlc-a", opts.namespace)
	}

	if opts.serviceAccount != (types.NamespacedName{Namespace: "app", Name: "workload"}) {
		t.Fatalf("serviceAccount = %#v", opts.serviceAccount)
	}

	if opts.awsServiceAccountRoleName != "role-a" {
		t.Fatalf("awsServiceAccountRoleName = %q", opts.awsServiceAccountRoleName)
	}

	if opts.clusterProfileProviderFile != "/tmp/providers.json" {
		t.Fatalf("clusterProfileProviderFile = %q", opts.clusterProfileProviderFile)
	}

	if opts.region != "ap-northeast-1" {
		t.Fatalf("region = %q", opts.region)
	}

	if opts.tokenExpiration != 10*time.Minute {
		t.Fatalf("tokenExpiration = %s", opts.tokenExpiration)
	}

	if opts.sessionDuration != time.Hour {
		t.Fatalf("sessionDuration = %s", opts.sessionDuration)
	}

	if opts.sessionName != "app-workload" {
		t.Fatalf("sessionName = %q", opts.sessionName)
	}

	if opts.kubeconfig != "/tmp/kubeconfig" {
		t.Fatalf("kubeconfig = %q", opts.kubeconfig)
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
				"--session-name", "app-workload",
			},
			want: "--clusterprofile-provider-file is required",
		},
		{
			name: "session name",
			args: []string{
				"--namespace", "wlc-a",
				"--service-account", "app/workload",
				"--clusterprofile-provider-file", "/tmp/providers.json",
			},
			want: "--session-name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseOptions(tt.args)
			if err == nil {
				t.Fatal("parseOptions returned nil error")
			}

			if err.Error() != tt.want {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}
