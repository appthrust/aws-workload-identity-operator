package oidc

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
)

func TestGenerateRSAKeyPEM(t *testing.T) {
	priv, pub, keyID, err := GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	if keyID == "" {
		t.Fatal("expected non-empty keyID")
	}

	keyIDBytes, err := base64.RawURLEncoding.DecodeString(keyID)
	if err != nil {
		t.Fatalf("keyID is not base64url encoded: %v", err)
	}

	if len(keyIDBytes) != 32 {
		t.Fatalf("expected keyID to encode a full SHA-256 digest, got %d bytes", len(keyIDBytes))
	}

	if block, _ := pem.Decode(priv); block == nil {
		t.Fatal("private key is not PEM-encoded")
	}

	block, _ := pem.Decode(pub)
	if block == nil {
		t.Fatal("public key is not PEM-encoded")
	}

	if _, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
		t.Fatalf("public key cannot be parsed: %v", err)
	}
}

func TestRenderIssuerObjects(t *testing.T) {
	_, pub, keyID, err := GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	objects, err := RenderIssuerObjects("https://issuer.example.com/", pub, keyID)
	if err != nil {
		t.Fatal(err)
	}

	if len(objects) != 2 {
		t.Fatalf("expected two issuer objects, got %d", len(objects))
	}

	assertDiscoveryObject(t, objects[0])
	assertJWKSObject(t, objects[1], pub, keyID)
}

func assertDiscoveryObject(t *testing.T, object IssuerObject) {
	t.Helper()

	if object.Key != DiscoveryObjectKey || object.ContentType != "application/json" {
		t.Fatalf("unexpected discovery object metadata: %#v", object)
	}

	var discovery map[string]any
	if err := json.Unmarshal(object.Body, &discovery); err != nil {
		t.Fatal(err)
	}

	if discovery["issuer"] != "https://issuer.example.com" || discovery["jwks_uri"] != "https://issuer.example.com/keys.json" {
		t.Fatalf("unexpected discovery document: %#v", discovery)
	}
}

func assertJWKSObject(t *testing.T, object IssuerObject, pub []byte, keyID string) {
	t.Helper()

	if object.Key != JWKSObjectKey || object.ContentType != "application/json" {
		t.Fatalf("unexpected JWKS object metadata: %#v", object)
	}

	var jwks struct {
		Keys []struct {
			KeyType   string `json:"kty"`
			PublicUse string `json:"use"`
			Algorithm string `json:"alg"`
			KeyID     string `json:"kid"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(object.Body, &jwks); err != nil {
		t.Fatal(err)
	}

	if len(jwks.Keys) != 1 {
		t.Fatalf("expected one JWK, got %d", len(jwks.Keys))
	}

	key := jwks.Keys[0]
	if key.KeyType != "RSA" || key.PublicUse != "sig" || key.Algorithm != "RS256" || key.KeyID != keyID {
		t.Fatalf("unexpected JWK metadata: %#v", key)
	}

	publicKey, err := ParseRSAPublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}

	wantExponent := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes())
	if key.Modulus == "" || key.Exponent != wantExponent {
		t.Fatalf("unexpected JWK key material: %#v", key)
	}
}

func TestKeyIDFromPublicKeyPEMMatchesGeneratedKeyID(t *testing.T) {
	_, pub, keyID, err := GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	got, err := KeyIDFromPublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}

	if got != keyID {
		t.Fatalf("expected key ID %q, got %q", keyID, got)
	}
}
