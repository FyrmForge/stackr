package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
)

var testKey = strings.Repeat("ab", 32)

// B0: New builds every leaf and flow exactly once.
func TestNewBuildsEachOnce(t *testing.T) {
	counts := map[string]int{}
	onBuild = func(name string) { counts[name]++ }
	t.Cleanup(func() { onBuild = func(string) {} })

	o, err := New(Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"}, WithDocker(dockerfake.New()), WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	for _, name := range []string{"store", "sessions", "leaf/user", "leaf/org", "leaf/stack", "leaf/environment",
		"leaf/tile", "leaf/image", "leaf/params", "leaf/volume", "leaf/domain", "leaf/credential", "leaf/connector",
		"leaf/managed", "leaf/release", "leaf/job", "leaf/backup", "leaf/settings", "flow/managed",
		"leaf/domain.Syncer", "flow/deploy", "flow/promote", "flow/backup", "flow/container", "flow/imagewatch",
		"flow/upgrade", "flow/jobs", "flow/schedule", "leaf/traffic", "flow/traffic"} {
		if counts[name] == 0 {
			t.Errorf("%s never built", name)
		}
	}
	for name, n := range counts {
		if n != 1 {
			t.Errorf("%s built %d times, want 1", name, n)
		}
	}
	if err := o.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestNewRefusesWithoutKey(t *testing.T) {
	_, err := New(Config{DataDir: t.TempDir()}, WithDocker(dockerfake.New()))
	if !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("New without key: %v, want ErrNoKey", err)
	}
}
