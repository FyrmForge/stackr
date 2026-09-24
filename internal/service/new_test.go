package service

import (
	"context"
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

	o, err := New(Config{DataDir: t.TempDir(), SecretsKey: testKey}, WithDocker(dockerfake.New()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	for _, name := range []string{"store", "sessions", "leaf/user", "leaf/settings"} {
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
