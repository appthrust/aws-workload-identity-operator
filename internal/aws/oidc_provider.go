package aws

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var oidcProviderARNPattern = regexp.MustCompile(`^arn:aws[a-z0-9-]*:iam::\d{12}:oidc-provider/(.+)$`)

// NormalizeIssuerURL returns the IAM OIDC provider host/path for a canonical
// HTTPS issuer URL. The returned value intentionally omits the https:// scheme
// because IAM trust policy condition keys use host/path.
func NormalizeIssuerURL(raw string) (string, error) {
	issuer, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse issuer URL: %w", err)
	}

	if issuer.Scheme != "https" {
		return "", fmt.Errorf("issuer URL must use https scheme")
	}

	if issuer.Host == "" {
		return "", fmt.Errorf("issuer URL must include a host")
	}

	if issuer.User != nil {
		return "", fmt.Errorf("issuer URL must not include user info")
	}

	if issuer.RawQuery != "" || issuer.Fragment != "" {
		return "", fmt.Errorf("issuer URL must not include query or fragment")
	}

	issuerHostPath := issuer.Host + issuer.EscapedPath()
	if err := validateIssuerHostPath(issuerHostPath); err != nil {
		return "", err
	}

	return issuerHostPath, nil
}

// OIDCProviderARNIssuerHostPath extracts the provider host/path from an IAM
// OpenIDConnectProvider ARN.
func OIDCProviderARNIssuerHostPath(arn string) (string, error) {
	matches := oidcProviderARNPattern.FindStringSubmatch(arn)
	if len(matches) != 2 || matches[1] == "" {
		return "", fmt.Errorf("invalid IAM OIDC provider ARN %q", arn)
	}

	issuerHostPath := matches[1]
	if err := validateIssuerHostPath(issuerHostPath); err != nil {
		return "", fmt.Errorf("invalid IAM OIDC provider ARN %q: %w", arn, err)
	}

	return issuerHostPath, nil
}

// ValidateOIDCProviderARNMatchesIssuer verifies that an external provider ARN
// belongs to the same normalized issuer host/path used in trust policies.
func ValidateOIDCProviderARNMatchesIssuer(arn, issuerHostPath string) error {
	providerHostPath, err := OIDCProviderARNIssuerHostPath(arn)
	if err != nil {
		return err
	}

	if providerHostPath != issuerHostPath {
		return fmt.Errorf("OIDC provider ARN path %q does not match issuer %q", providerHostPath, issuerHostPath)
	}

	return nil
}

func validateIssuerHostPath(issuerHostPath string) error {
	if issuerHostPath == "" {
		return fmt.Errorf("issuer host/path must not be empty")
	}

	if strings.Contains(issuerHostPath, "://") {
		return fmt.Errorf("issuer host/path must not include a scheme")
	}

	if strings.ContainsAny(issuerHostPath, " \t\r\n") {
		return fmt.Errorf("issuer host/path must not contain whitespace")
	}

	if strings.ContainsAny(issuerHostPath, "?#") {
		return fmt.Errorf("issuer host/path must not include query or fragment")
	}

	if strings.HasSuffix(issuerHostPath, "/") {
		return fmt.Errorf("issuer host/path must not end with a slash")
	}

	issuer, err := url.Parse("https://" + issuerHostPath)
	if err != nil {
		return fmt.Errorf("parse issuer host/path: %w", err)
	}

	if issuer.Host == "" {
		return fmt.Errorf("issuer host/path must include a host")
	}

	if issuer.User != nil {
		return fmt.Errorf("issuer host/path must not include user info")
	}

	if normalized := issuer.Host + issuer.EscapedPath(); normalized != issuerHostPath {
		return fmt.Errorf("issuer host/path must be canonical")
	}

	return nil
}
