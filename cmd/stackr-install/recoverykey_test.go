package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// The install makes the recovery key pair once: the private half is returned
// to be printed, only the public half and generation 1 stay on the box, and a
// re-run makes nothing and prints nothing.
func TestRecoveryKeyMadeOnce(t *testing.T) {
	dir := t.TempDir()
	r := runner{out: io.Discard}
	key, err := r.recoveryKey(dir)
	if err != nil || !strings.HasPrefix(key, "AGE-SECRET-KEY-") {
		t.Fatalf("recoveryKey = %q, %v", key, err)
	}
	pub, gen, err := installspec.LoadRecovery(dir)
	if err != nil || pub == "" || gen != 1 {
		t.Fatalf("recovery.pub = %q gen %d, %v", pub, gen, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, installspec.RecoveryPubFile))
	if strings.Contains(string(b), key) {
		t.Error("the private half was written to recovery.pub")
	}
	if again, err := r.recoveryKey(dir); err != nil || again != "" {
		t.Errorf("re-run = %q, %v", again, err)
	}
}
