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
const prefix = "enc2:"

// legacyPrefix marks values sealed before labels existed.
const legacyPrefix = "enc1:"

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

// Encrypt returns "enc2:<base64(nonce||ciphertext)>". label is bound in as
// the GCM additional data, so a value copied to another row, column or key
// fails to open. The empty string stays empty: there is nothing to hide and
// nothing to decrypt later.
func (b *Box) Encrypt(s, label string) (string, error) {
	if s == "" {
		return "", nil
	}
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: nonce: %w", err)
	}
	return prefix + base64.StdEncoding.EncodeToString(b.gcm.Seal(nonce, nonce, []byte(s), []byte(label))), nil
}

// Decrypt reverses Encrypt under the same label. A value without the enc2
// prefix is an error: no plaintext rows exist, so it is corruption.
func (b *Box) Decrypt(s, label string) (string, error) {
	return b.open(s, prefix, []byte(label))
}

// DecryptLegacy opens an enc1 value, which carries no label. Only the upgrade
// sweep uses it.
func (b *Box) DecryptLegacy(s string) (string, error) {
	return b.open(s, legacyPrefix, nil)
}

func (b *Box) open(s, pre string, aad []byte) (string, error) {
	if s == "" {
		return "", nil
	}
	body, ok := strings.CutPrefix(s, pre)
	if !ok {
		return "", fmt.Errorf("%w: not an %s value", ErrDecrypt, strings.TrimSuffix(pre, ":"))
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil || len(raw) < b.gcm.NonceSize() {
		return "", fmt.Errorf("%w: corrupt value", ErrDecrypt)
	}
	n := b.gcm.NonceSize()
	pt, err := b.gcm.Open(nil, raw[:n], raw[n:], aad)
	if err != nil {
		return "", fmt.Errorf("%w: wrong key, wrong place or tampered value", ErrDecrypt)
	}
	return string(pt), nil
}

// Sealed reports whether s looks like an Encrypt result (either version), so
// a sweep can tell sealed settings from plain ones.
func Sealed(s string) bool { return strings.HasPrefix(s, prefix) || Legacy(s) }

// Legacy reports whether s is an unlabelled enc1 value.
func Legacy(s string) bool { return strings.HasPrefix(s, legacyPrefix) }
