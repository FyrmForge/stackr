package secrets

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const (
	keyA = "0000000000000000000000000000000000000000000000000000000000000001"
	keyB = "abababababababababababababababababababababababababababababababab"
)

func box(t *testing.T, key string) *Box {
	t.Helper()
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := box(t, keyA)
	for _, in := range []string{"hunter2", "KEY=value\nMULTI=\"a\nb\"", ""} {
		enc, err := b.Encrypt(in, "x/1/0")
		if err != nil {
			t.Fatal(err)
		}
		if in != "" && !strings.HasPrefix(enc, "enc2:") {
			t.Errorf("Encrypt(%q) = %q, not encrypted", in, enc)
		}
		got, err := b.Decrypt(enc, "x/1/0")
		if err != nil || got != in {
			t.Errorf("Decrypt(Encrypt(%q)) = %q, %v", in, got, err)
		}
	}
}

func TestDecryptFailsTyped(t *testing.T) {
	enc, err := box(t, keyA).Encrypt("hunter2", "x/1/0")
	if err != nil {
		t.Fatal(err)
	}
	b := box(t, keyB)
	enc2, _ := box(t, keyB).Encrypt("hunter2", "x/1/0")
	for name, in := range map[string]string{
		"wrong key":   enc,
		"plaintext":   "hunter2",
		"not base64":  "enc2:!!!",
		"truncated":   "enc2:AAAA",
		"wrong label": "",
		"legacy":      legacy(t, box(t, keyB), "hunter2"),
	} {
		if name == "wrong label" {
			in = enc2
		}
		label := "x/1/0"
		if name == "wrong label" {
			label = "x/2/0"
		}
		if _, err := b.Decrypt(in, label); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: err = %v, want ErrDecrypt", name, err)
		}
	}
}

func TestNewRefusesBadKeys(t *testing.T) {
	if _, err := New(""); !errors.Is(err, ErrNoKey) {
		t.Errorf("empty key: err = %v, want ErrNoKey", err)
	}
	for _, k := range []string{"zz", keyA[:62], keyA + "00"} {
		if _, err := New(k); err == nil {
			t.Errorf("New(%q) accepted a bad key", k)
		}
	}
}

// legacy builds an enc1 value the way the old Encrypt did: no AAD.
func legacy(t *testing.T, b *Box, s string) string {
	t.Helper()
	nonce := make([]byte, b.gcm.NonceSize())
	return legacyPrefix + base64.StdEncoding.EncodeToString(b.gcm.Seal(nonce, nonce, []byte(s), nil))
}

func TestLegacy(t *testing.T) {
	b := box(t, keyA)
	old := legacy(t, b, "hunter2")
	if !Sealed(old) || !Legacy(old) {
		t.Error("enc1 not recognised")
	}
	if got, err := b.DecryptLegacy(old); err != nil || got != "hunter2" {
		t.Errorf("DecryptLegacy = %q, %v", got, err)
	}
	if _, err := b.Decrypt(old, "x/1/0"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Decrypt(enc1) err = %v, want ErrDecrypt", err)
	}
}
