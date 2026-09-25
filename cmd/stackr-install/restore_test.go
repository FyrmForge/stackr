package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func archive(t *testing.T, pass string, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkFactor(10)
	enc, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"stackr.db", "keys/master.key", "VERSION", "../evil"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	for _, c := range []interface{ Close() error }{tw, gz, enc} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return &buf
}

func TestUnpack(t *testing.T) {
	good := map[string]string{"stackr.db": "db", "keys/master.key": "k\n", "VERSION": "v0.1.0\n"}
	dir := t.TempDir()
	ver, err := unpack(archive(t, "pw", good), "pw", dir)
	if err != nil || ver != "v0.1.0" {
		t.Fatalf("unpack = %q %v", ver, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keys/master.key")); string(b) != "k\n" {
		t.Errorf("master key = %q", b)
	}

	if _, err := unpack(archive(t, "pw", good), "wrong", t.TempDir()); err == nil {
		t.Error("wrong passphrase accepted")
	}
	evil := map[string]string{
		"stackr.db":       "db",
		"keys/master.key": "k",
		"VERSION":         "v0.1.0",
		"../evil":         "x",
	}
	if _, err := unpack(archive(t, "pw", evil), "pw", t.TempDir()); err == nil {
		t.Error("entry outside the three names accepted")
	}
	if _, err := unpack(archive(t, "pw", map[string]string{"VERSION": "v0.1.0"}), "pw", t.TempDir()); err == nil {
		t.Error("archive without a database accepted")
	}
}
