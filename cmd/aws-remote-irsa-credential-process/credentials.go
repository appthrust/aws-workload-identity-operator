package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
)

type credentialProvider interface {
	Retrieve(context.Context) (awssdk.Credentials, error)
}

//nolint:tagliatelle // AWS credential_process requires these exact JSON field names.
type credentialProcessOutput struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration,omitempty"`
}

func retrieveCredentials(ctx context.Context, opts *options, deps dependencies) (awssdk.Credentials, error) {
	accessConfig, err := deps.loadAccessConfig(opts.clusterProfileProviderFile)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("load clusterprofile provider file: %w", err)
	}

	hubConfig, err := deps.buildHubConfig(opts.kubeconfig)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("build hub kubeconfig: %w", err)
	}

	hubClient, err := deps.newHubClient(hubConfig)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("create hub API client: %w", err)
	}

	hubKubeClient, err := deps.newHubKubeClient(hubConfig)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("create hub Kubernetes client: %w", err)
	}

	provider, err := deps.newProvider(remoteIRSAOptions{
		HubReader:                  hubClient,
		HubKubeClient:              hubKubeClient,
		ClusterProfileAccessConfig: accessConfig,
		WorkloadNamespace:          opts.namespace,
		AWSServiceAccountRoleName:  opts.awsServiceAccountRoleName,
		ServiceAccount:             opts.serviceAccount,
		Region:                     opts.region,
		TokenExpiration:            opts.tokenExpiration,
		SessionDuration:            opts.sessionDuration,
		SessionName:                opts.sessionName,
	})
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("create remote IRSA provider: %w", err)
	}

	credentials, err := provider.Retrieve(ctx)
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("retrieve remote IRSA credentials: %w", err)
	}

	return credentials, nil
}

func writeCredentialProcessOutput(w io.Writer, creds *awssdk.Credentials) error {
	output := credentialProcessOutput{
		Version:         1,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
	}

	if creds.CanExpire && !creds.Expires.IsZero() {
		output.Expiration = creds.Expires.UTC().Format(time.RFC3339)
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(output); err != nil { //nolint:gosec // credential_process intentionally writes session credentials to stdout.
		return fmt.Errorf("encode credential_process output: %w", err)
	}

	return nil
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}

	message := err.Error()
	normalizedMessage := strings.ToLower(message)

	redactions := []string{
		"accesskeyid",
		"access_key_id",
		"secretaccesskey",
		"secret_access_key",
		"sessiontoken",
		"session_token",
		"bearertoken",
		"webidentitytoken",
		"web_identity_token",
	}

	for _, marker := range redactions {
		if strings.Contains(normalizedMessage, marker) {
			return "remote IRSA credential_process failed"
		}
	}

	return message
}
