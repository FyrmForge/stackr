package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

// A relative DataDir becomes absolute: a relative bind source is a volume
// name to Docker.
func TestDataDirIsAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, t.TempDir())
	if err != nil {
		t.Skip(err)
	}
	orch, err := New(
		Config{DataDir: rel, SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	if !filepath.IsAbs(orch.cfg.DataDir) {
		t.Errorf("DataDir = %q", orch.cfg.DataDir)
	}
}
