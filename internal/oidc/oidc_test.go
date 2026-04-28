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

func TestRenderIssuerPublicationDigestChangesWithObjectIdentity(t *testing.T) {
	_, pub, keyID, err := GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	baseline, err := RenderIssuerPublication("https://issuer.example.com", pub, keyID)
	if err != nil {
		t.Fatal(err)
	}

	differentIssuer, err := RenderIssuerPublication("https://other.example.com", pub, keyID)
	if err != nil {
		t.Fatal(err)
	}

	differentKeyID, err := RenderIssuerPublication("https://issuer.example.com", pub, keyID+"x")
	if err != nil {
		t.Fatal(err)
	}

	assertDifferentDigest(t, baseline.ObjectSetDigest, differentIssuer.ObjectSetDigest, "issuer URL")
	assertDifferentDigest(t, baseline.ObjectSetDigest, differentKeyID.ObjectSetDigest, "key ID")

	bodyChanged := cloneIssuerObjects(baseline.Objects)
	bodyChanged[0].Body = append([]byte(nil), bodyChanged[0].Body...)
	bodyChanged[0].Body = append(bodyChanged[0].Body, '\n')
	assertDifferentDigest(t, baseline.ObjectSetDigest, computeIssuerObjectSetDigest(bodyChanged), "body")

	contentTypeChanged := cloneIssuerObjects(baseline.Objects)
	contentTypeChanged[0].ContentType = "application/openid+json"
	assertDifferentDigest(t, baseline.ObjectSetDigest, computeIssuerObjectSetDigest(contentTypeChanged), "content type")

	keyChanged := cloneIssuerObjects(baseline.Objects)
	keyChanged[0].Key = "openid-configuration"
	assertDifferentDigest(t, baseline.ObjectSetDigest, computeIssuerObjectSetDigest(keyChanged), "object key")
}

func TestIssuerPublicationDigestStableIndependentOfObjectOrder(t *testing.T) {
	_, pub, keyID, err := GenerateRSAKeyPEM(2048)
	if err != nil {
		t.Fatal(err)
	}

	publication, err := RenderIssuerPublication("https://issuer.example.com", pub, keyID)
	if err != nil {
		t.Fatal(err)
	}

	reordered := []IssuerObject{publication.Objects[1], publication.Objects[0]}
	if got := computeIssuerObjectSetDigest(reordered); got != publication.ObjectSetDigest {
		t.Fatalf("expected reordered object set digest %q, got %q", publication.ObjectSetDigest, got)
	}
}

func assertDifferentDigest(t *testing.T, baseline, candidate, field string) {
	t.Helper()

	if baseline == "" || candidate == "" {
		t.Fatalf("expected non-empty digests for %s: baseline=%q candidate=%q", field, baseline, candidate)
	}

	if baseline == candidate {
		t.Fatalf("expected %s change to alter digest %q", field, baseline)
	}
}

func cloneIssuerObjects(objects []IssuerObject) []IssuerObject {
	out := make([]IssuerObject, len(objects))
	copy(out, objects)

	for i := range out {
		out[i].Body = append([]byte(nil), out[i].Body...)
	}

	return out
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
