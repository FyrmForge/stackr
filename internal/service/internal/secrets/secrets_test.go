package secrets

import (
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
		enc, err := b.Encrypt(in)
		if err != nil {
			t.Fatal(err)
		}
		if in != "" && !strings.HasPrefix(enc, "enc1:") {
			t.Errorf("Encrypt(%q) = %q, not encrypted", in, enc)
		}
		got, err := b.Decrypt(enc)
		if err != nil || got != in {
			t.Errorf("Decrypt(Encrypt(%q)) = %q, %v", in, got, err)
		}
	}
}

func TestDecryptFailsTyped(t *testing.T) {
	enc, err := box(t, keyA).Encrypt("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	b := box(t, keyB)
	for name, in := range map[string]string{
		"wrong key":      enc,
		"plaintext":      "hunter2",
		"not base64":     "enc1:!!!",
		"truncated":      "enc1:AAAA",
		"future version": "enc2:" + strings.TrimPrefix(enc, "enc1:"),
	} {
		if _, err := b.Decrypt(in); !errors.Is(err, ErrDecrypt) {
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
