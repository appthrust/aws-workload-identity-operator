package oidc

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
)

const (
	// DiscoveryObjectKey is the S3 object key for the OpenID Provider metadata.
	DiscoveryObjectKey = ".well-known/openid-configuration"
	// JWKSObjectKey is the S3 object key for the issuer JSON Web Key Set.
	JWKSObjectKey = "keys.json"
)

// IssuerObject is one public JSON object that backs the self-hosted issuer.
type IssuerObject struct {
	Key         string
	ContentType string
	Body        []byte
}

//nolint:tagliatelle // OpenID Provider Configuration field names are specified as snake_case.
type discoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KeyType   string `json:"kty"`
	PublicUse string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
}

// RenderIssuerObjects renders the two JSON documents served by the self-hosted
// OIDC issuer.
func RenderIssuerObjects(issuerURL string, publicKeyPEM []byte, keyID string) ([]IssuerObject, error) {
	discovery, err := RenderDiscoveryDocument(issuerURL)
	if err != nil {
		return nil, fmt.Errorf("render discovery document: %w", err)
	}

	jwks, err := RenderJWKS(publicKeyPEM, keyID)
	if err != nil {
		return nil, fmt.Errorf("render JWKS: %w", err)
	}

	return []IssuerObject{
		{Key: DiscoveryObjectKey, ContentType: "application/json", Body: discovery},
		{Key: JWKSObjectKey, ContentType: "application/json", Body: jwks},
	}, nil
}

// RenderDiscoveryDocument renders the OpenID Provider Configuration document.
func RenderDiscoveryDocument(issuerURL string) ([]byte, error) {
	issuerURL = strings.TrimRight(issuerURL, "/")
	if issuerURL == "" {
		return nil, fmt.Errorf("issuer URL is empty")
	}

	body, err := json.Marshal(discoveryDocument{
		Issuer:                           issuerURL,
		JWKSURI:                          issuerURL + "/" + JWKSObjectKey,
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"RS256"},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal discovery document: %w", err)
	}

	return body, nil
}

// RenderJWKS renders a JSON Web Key Set from a PKIX RSA public key PEM.
func RenderJWKS(publicKeyPEM []byte, keyID string) ([]byte, error) {
	if keyID == "" {
		return nil, fmt.Errorf("key ID is empty")
	}

	publicKey, err := ParseRSAPublicKeyPEM(publicKeyPEM)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(jwksDocument{
		Keys: []jwk{{
			KeyType:   "RSA",
			PublicUse: "sig",
			Algorithm: "RS256",
			KeyID:     keyID,
			Modulus:   base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
			Exponent:  base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal JWKS: %w", err)
	}

	return body, nil
}

// ParseRSAPublicKeyPEM parses a PKIX RSA public key PEM.
func ParseRSAPublicKeyPEM(publicKeyPEM []byte) (*rsa.PublicKey, error) {
	publicKey, _, err := parseRSAPublicKeyPEMBlock(publicKeyPEM)

	return publicKey, err
}

// KeyIDFromPublicKeyPEM returns the deterministic key ID used for generated
// self-hosted issuer keys.
func KeyIDFromPublicKeyPEM(publicKeyPEM []byte) (string, error) {
	_, derBytes, err := parseRSAPublicKeyPEMBlock(publicKeyPEM)
	if err != nil {
		return "", err
	}

	return keyIDFromPublicDER(derBytes), nil
}

func parseRSAPublicKeyPEMBlock(publicKeyPEM []byte) (*rsa.PublicKey, []byte, error) {
	block, _ := pem.Decode(publicKeyPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("public key is not PEM-encoded")
	}

	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse public key: %w", err)
	}

	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("public key is %T, not *rsa.PublicKey", parsed)
	}

	return publicKey, block.Bytes, nil
}

func keyIDFromPublicDER(publicDER []byte) string {
	sum := sha256.Sum256(publicDER)

	return base64.RawURLEncoding.EncodeToString(sum[:])
}
