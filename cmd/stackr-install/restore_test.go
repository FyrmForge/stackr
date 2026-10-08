package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	for _, name := range []string{"stackr.db", "keys/master.key", "VERSION", "caddy.tar.gz", "../evil"} {
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

// An archive with the proxy's volume stages it; one without is as before.
// restoreCaddy stops the proxy, untars into its volume, starts it.
func TestRestoreCaddy(t *testing.T) {
	files := map[string]string{"stackr.db": "db", "keys/master.key": "k\n", "VERSION": "v0.1.0\n", "caddy.tar.gz": "tarball"}
	dir := t.TempDir()
	if _, err := unpack(archive(t, "pw", files), "pw", dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "caddy.tar.gz")); string(b) != "tarball" {
		t.Errorf("staged caddy = %q", b)
	}
	var out bytes.Buffer
	if err := restoreCaddy(context.Background(), runner{dry: true, out: &out}, "img:1", dir); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	stop, untar, start := strings.Index(got, "stop stackr-proxy"), strings.Index(got, "stackr-caddy:/data"), strings.Index(got, "start stackr-proxy")
	if stop < 0 || untar < stop || start < untar {
		t.Errorf("order wrong:\n%s", got)
	}
	empty := t.TempDir()
	out.Reset()
	if err := restoreCaddy(context.Background(), runner{dry: true, out: &out}, "img:1", empty); err != nil || out.Len() != 0 {
		t.Errorf("no caddy member: %v %q", err, out.String())
	}
}

// untarScript wipes and untars; when the untar fails the tree that was there
// comes back, so a bad tarball never costs the certificates.
func TestUntarScriptKeepsTreeOnFailure(t *testing.T) {
	run := func(dir string, stdin []byte) error {
		cmd := exec.Command("sh", "-c", untarScript(dir))
		cmd.Stdin = bytes.NewReader(stdin)
		return cmd.Run()
	}
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "caddy", "certificates"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "caddy", "certificates", "a.crt"), []byte("pem"), 0o600))
	must(os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o600))

	if err := run(dir, []byte("not a tarball")); err == nil {
		t.Error("a bad tarball reported success")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "caddy", "certificates", "a.crt")); string(b) != "pem" {
		t.Errorf("the old tree was not put back: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".hidden")); string(b) != "h" {
		t.Errorf("the old dotfile was not put back: %q", b)
	}

	// A good tarball replaces the tree.
	src := t.TempDir()
	must(os.WriteFile(filepath.Join(src, "new.crt"), []byte("new"), 0o600))
	var tarball bytes.Buffer
	cmd := exec.Command("tar", "-czf", "-", "-C", src, ".")
	cmd.Stdout = &tarball
	must(cmd.Run())
	must(run(dir, tarball.Bytes()))
	if _, err := os.Stat(filepath.Join(dir, "caddy")); err == nil {
		t.Error("the old tree survived a good restore")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new.crt")); string(b) != "new" {
		t.Errorf("new tree = %q", b)
	}
}

// The staging dir holds unencrypted certificate keys; a restore that went
// through deletes it.
func TestDropStage(t *testing.T) {
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "caddy.tar.gz"), []byte("keys"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dropStage(stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); err == nil {
		t.Error("the staging dir is still there")
	}
}
