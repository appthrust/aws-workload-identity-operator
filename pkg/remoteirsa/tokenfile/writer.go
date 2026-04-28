package tokenfile

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// AWSConfig is the shared config profile written next to the web identity token.
type AWSConfig struct {
	RoleARN              string
	WebIdentityTokenFile string
	RoleSessionName      string
}

const secureFileMode fs.FileMode = 0o600

func WriteTokenAtomic(path, token string) error {
	if token == "" {
		return fmt.Errorf("token is empty")
	}
	if path == "" {
		return fmt.Errorf("token file path is empty")
	}

	return writeFileAtomic(path, []byte(token), secureFileMode)
}

// WriteAWSConfigAtomic writes an AWS shared config file atomically.
func WriteAWSConfigAtomic(path string, cfg AWSConfig) error {
	switch {
	case path == "":
		return fmt.Errorf("AWS config file path is empty")
	case cfg.RoleARN == "":
		return fmt.Errorf("role ARN is empty")
	case cfg.WebIdentityTokenFile == "":
		return fmt.Errorf("web identity token file path is empty")
	case cfg.RoleSessionName == "":
		return fmt.Errorf("role session name is empty")
	}

	var buf bytes.Buffer
	_, _ = fmt.Fprintln(&buf, "[default]")
	_, _ = fmt.Fprintf(&buf, "role_arn = %s\n", cfg.RoleARN)
	_, _ = fmt.Fprintf(&buf, "web_identity_token_file = %s\n", cfg.WebIdentityTokenFile)
	_, _ = fmt.Fprintf(&buf, "role_session_name = %s\n", cfg.RoleSessionName)
	_, _ = fmt.Fprintln(&buf, "sts_regional_endpoints = regional")

	return writeFileAtomic(path, buf.Bytes(), secureFileMode)
}

func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create file directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".aws-irsa-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}

	return nil
}
