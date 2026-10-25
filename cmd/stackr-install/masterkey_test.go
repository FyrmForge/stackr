package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// Restoring an archive that carries another master key than the box runs on
// warns, and names the key that replaced it once the box has rotated.
func TestMasterKeyChangeWarnsOnOlderKey(t *testing.T) {
	data, stage := t.TempDir(), t.TempDir()
	put := func(dir, name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(data, installspec.KeyFile, "new\n")
	put(stage, installspec.KeyFile, "new\n")
	if w := masterKeyChange(data, stage); w != "" {
		t.Errorf("same key warned: %q", w)
	}
	put(stage, installspec.KeyFile, "old\n")
	if w := masterKeyChange(data, stage); !strings.Contains(w, "different master key") {
		t.Errorf("warning = %q", w)
	}
	put(data, "keys/master.gen", "3\n2026-10-20T10:00:00Z\n")
	want := "This backup uses an older master key; key 3 replaced it on 2026-10-20; restoring brings the old key back."
	if w := masterKeyChange(data, stage); w != want {
		t.Errorf("warning = %q", w)
	}
}
