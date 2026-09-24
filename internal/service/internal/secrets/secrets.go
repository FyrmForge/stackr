// Package secrets encrypts values at rest with AES-GCM. There is no package
// state: a Box exists only when a key was loaded, so "encrypt before load"
// cannot be written, and a missing key is an error at startup, never a
// silent plaintext write.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// prefix versions the storage encoding, so a key rotation or a new algorithm
// can tell old bytes from new.
const prefix = "enc1:"

var (
	// ErrNoKey is returned by New when no master key is configured.
	ErrNoKey = errors.New("secrets: no master key configured")
	// ErrDecrypt covers every way a stored value fails to open: wrong key,
	// missing prefix, corrupt or truncated body. Never partial plaintext.
	ErrDecrypt = errors.New("secrets: cannot decrypt value")
)

// Box holds the cipher for one master key.
type Box struct{ gcm cipher.AEAD }

// New builds a Box from a 64-hex-character (32-byte) key.
func New(hexKey string) (*Box, error) {
	hexKey = strings.TrimSpace(hexKey)
	if hexKey == "" {
		return nil, ErrNoKey
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("secrets: master key must be 64 hex characters")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

// Encrypt returns "enc1:<base64(nonce||ciphertext)>". The empty string stays
// empty: there is nothing to hide and nothing to decrypt later.
func (b *Box) Encrypt(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: nonce: %w", err)
	}
	return prefix + base64.StdEncoding.EncodeToString(b.gcm.Seal(nonce, nonce, []byte(s), nil)), nil
}

// Decrypt reverses Encrypt. A value without the prefix is an error: no
// plaintext rows exist, so an unprefixed value is corruption.
func (b *Box) Decrypt(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	body, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return "", fmt.Errorf("%w: not an enc1 value", ErrDecrypt)
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil || len(raw) < b.gcm.NonceSize() {
		return "", fmt.Errorf("%w: corrupt value", ErrDecrypt)
	}
	n := b.gcm.NonceSize()
	pt, err := b.gcm.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", fmt.Errorf("%w: wrong key or tampered value", ErrDecrypt)
	}
	return string(pt), nil
}
