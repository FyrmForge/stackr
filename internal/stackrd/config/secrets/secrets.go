// Package secrets encrypts sensitive values (app/db env blobs) at rest with
// AES-GCM. The master key lives in <dataDir>/keys/master.key (0600), created
// on first boot; STACKR_MASTER_KEY (64 hex chars) overrides it so the key can
// live apart from the database.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "enc:"

var gcm cipher.AEAD

// master is the loaded key, kept so a panel backup can carry it. Without it
// the database in the archive is unreadable ciphertext on any other host.
var master []byte

// Load initializes the cipher from STACKR_MASTER_KEY or the key file at path,
// generating the file if absent.
func Load(path string) error {
	var key []byte
	if h := os.Getenv("STACKR_MASTER_KEY"); h != "" {
		k, err := hex.DecodeString(h)
		if err != nil || len(k) != 32 {
			return fmt.Errorf("STACKR_MASTER_KEY must be 64 hex chars")
		}
		key = k
	} else {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			k, derr := hex.DecodeString(strings.TrimSpace(string(b)))
			if derr != nil || len(k) != 32 {
				return fmt.Errorf("invalid master key file %s", path)
			}
			key = k
		case os.IsNotExist(err):
			key = make([]byte, 32)
			if _, rerr := rand.Read(key); rerr != nil {
				return rerr
			}
			if merr := os.MkdirAll(filepath.Dir(path), 0o700); merr != nil {
				return merr
			}
			if werr := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); werr != nil {
				return werr
			}
		default:
			return err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err = cipher.NewGCM(block)
	if err != nil {
		return err
	}
	master = key
	return nil
}

// Key returns the master key in the same hex form the key file holds, so a
// backup can write it back out verbatim. Empty before Load.
func Key() string {
	if master == nil {
		return ""
	}
	return hex.EncodeToString(master)
}

// Encrypt returns "enc:<base64(nonce||ciphertext)>". Empty and already
// encrypted input pass through unchanged.
func Encrypt(s string) string {
	if s == "" || strings.HasPrefix(s, prefix) || gcm == nil {
		return s
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ct := gcm.Seal(nonce, nonce, []byte(s), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct)
}

// Decrypt reverses Encrypt. Values without the prefix (legacy plaintext) pass
// through unchanged.
func Decrypt(s string) (string, error) {
	if !strings.HasPrefix(s, prefix) {
		return s, nil
	}
	if gcm == nil {
		return "", fmt.Errorf("secrets: cipher not initialized")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("secrets: ciphertext too short")
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// RandomHex is n cryptographically random bytes, hex-encoded, the generator
// behind every credential stackr mints for itself: db passwords, registry
// passwords, webhook tokens, invite tokens.
//
// It panics rather than returning an error. A machine that cannot produce
// randomness must not go on to hand out a guessable password, and every caller
// would otherwise write the same "if err != nil { panic }" it replaces.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Generate mints a random string of exactly length characters from letters
// plus the opted-in classes, the generator behind `default: generated`
// config secrets. Same panic rule as RandomHex.
func Generate(length int, numbers, symbols bool) string {
	charset := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if numbers {
		charset += "0123456789"
	}
	if symbols {
		// Shell-, URL- and YAML-safe subset: these land in env vars and
		// connection strings assembled by apps that never quote.
		charset += "-_.~!@#%^*+="
	}
	out := make([]byte, length)
	max := big.NewInt(int64(len(charset)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = charset[n.Int64()]
	}
	return string(out)
}
