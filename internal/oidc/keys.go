// Package oidc generates RSA signing keys for self-hosted IRSA.
package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"k8s.io/client-go/util/keyutil"
)

// GenerateRSAKeyPEM creates a signing key pair and deterministic key ID.
func GenerateRSAKeyPEM(bits int) ([]byte, []byte, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate RSA key: %w", err)
	}

	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshal RSA public key: %w", err)
	}

	privatePEM, err := keyutil.MarshalPrivateKeyToPEM(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("encode RSA private key: %w", err)
	}

	publicPEM := pem.EncodeToMemory(&pem.Block{Type: keyutil.PublicKeyBlockType, Bytes: publicDER})
	keyID := keyIDFromPublicDER(publicDER)

	return privatePEM, publicPEM, keyID, nil
}
