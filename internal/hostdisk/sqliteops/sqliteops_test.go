package sqliteops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gone.db")
	if err := Checkpoint(context.Background(), p); err == nil {
		t.Error("checkpoint of a missing file succeeded")
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("checkpoint created the file")
	}
}
