package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSignJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemStr := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	tok, err := signJWT("12345", pemStr)
	require.NoError(t, err)
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3, "want 3 jwt parts")
	// Signature verifies against the public key.
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], sig), "signature does not verify")
	// Claims carry the app id as issuer.
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
	}
	require.NoError(t, json.Unmarshal(cb, &claims), "bad claims: %s", cb)
	require.Equal(t, "12345", claims.Iss, "bad claims: %s", cb)
}
