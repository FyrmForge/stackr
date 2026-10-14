package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/wgapply"
)

func TestWorkerWG(t *testing.T) {
	old := envDevMode
	t.Cleanup(func() { envDevMode = old })
	envDevMode = true
	t.Setenv("STACKR_FAKE_HOST", "true")
	if w, err := workerWG(); err != nil {
		t.Fatal(err)
	} else if _, ok := w.(*wgapply.Recorder); !ok {
		t.Error("fake host did not get the recorder")
	}
	t.Setenv("STACKR_FAKE_HOST", "")
	if w, err := workerWG(); err != nil {
		t.Fatal(err)
	} else if _, ok := w.(wgapply.Kernel); !ok {
		t.Error("real host did not get the kernel applier")
	}
}

// A worker refuses the fake host outside DEV_MODE, like the controller.
func TestWorkerWGFakeHostNeedsDevMode(t *testing.T) {
	old := envDevMode
	t.Cleanup(func() { envDevMode = old })
	envDevMode = false
	t.Setenv("STACKR_FAKE_HOST", "true")
	if _, err := workerWG(); err == nil {
		t.Fatal("STACKR_FAKE_HOST without DEV_MODE must be refused")
	}
}

func TestWorkerConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := workerConfig([]string{"--state-dir", dir}); err == nil {
		t.Fatal("no --addr was accepted")
	}
	if _, err := workerConfig([]string{"--state-dir", dir, "--addr", "10.207.255.2:7443"}); err == nil {
		t.Fatal("a state dir with no cert was accepted")
	}

	ca, err := clusterca.NewCA(time.Now())
	must(t, err)
	must(t, clusterca.SaveCA(dir, ca))
	key, err := clusterca.NewKey()
	must(t, err)
	csr, err := clusterca.NewCSR(key)
	must(t, err)
	iss, err := ca.IssueNode(csr, "w1", time.Now())
	must(t, err)
	kb, err := clusterca.KeyPEM(key)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "node.crt"), iss.CertPEM, 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "node.key"), kb, 0o600))
	// the CA cert is read from the state dir's own name
	b, err := os.ReadFile(filepath.Join(dir, clusterca.CACertFile))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "cluster-ca.crt"), b, 0o644))

	cfg, err := workerConfig([]string{"--state-dir", dir, "--addr", "10.207.255.2:7443"})
	must(t, err)
	if cfg.TLS == nil || cfg.Addr != "10.207.255.2:7443" || cfg.StateDir != dir || cfg.DockerSock != "/var/run/docker.sock" {
		t.Errorf("config = %+v", cfg)
	}
}
