package tokenfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteTokenAtomicWritesFinalFileWithSecureMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	if err := WriteTokenAtomic(path, "jwt-token"); err != nil {
		t.Fatalf("WriteTokenAtomic returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(data) != "jwt-token" {
		t.Fatalf("token file = %q", string(data))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
}

func TestWriteTokenAtomicRejectsEmptyToken(t *testing.T) {
	err := WriteTokenAtomic(filepath.Join(t.TempDir(), "token"), "")
	if err == nil {
		t.Fatal("WriteTokenAtomic returned nil error")
	}
	if err.Error() != "token is empty" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestWriteAWSConfigAtomicWritesDefaultProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aws-config")

	err := WriteAWSConfigAtomic(path, AWSConfig{
		RoleARN:              "arn:aws:iam::123456789012:role/karpenter",
		WebIdentityTokenFile: "/var/run/aws-irsa/token",
		RoleSessionName:      "awio-app-workload-123456789abc",
	})
	if err != nil {
		t.Fatalf("WriteAWSConfigAtomic returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read AWS config: %v", err)
	}
	want := `[default]
role_arn = arn:aws:iam::123456789012:role/karpenter
web_identity_token_file = /var/run/aws-irsa/token
role_session_name = awio-app-workload-123456789abc
sts_regional_endpoints = regional
`
	if string(data) != want {
		t.Fatalf("AWS config = %q, want %q", string(data), want)
	}
}

func TestWriteAWSConfigAtomicRequiresRoleSessionName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aws-config")

	err := WriteAWSConfigAtomic(path, AWSConfig{
		RoleARN:              "arn:aws:iam::123456789012:role/workload",
		WebIdentityTokenFile: "/var/run/aws-irsa/token",
	})
	if err == nil {
		t.Fatal("WriteAWSConfigAtomic returned nil error")
	}
	if err.Error() != "role session name is empty" {
		t.Fatalf("error = %q", err.Error())
	}
}
