package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

var testKey = strings.Repeat("ab", 32)

// B0: New builds every leaf and flow exactly once.
func TestNewBuildsEachOnce(t *testing.T) {
	counts := map[string]int{}
	onBuild = func(name string) { counts[name]++ }
	t.Cleanup(func() { onBuild = func(string) {} })

	orch, err := New(
		Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	for _, name := range []string{
		"store",
		"sessions",
		"leaf/user",
		"leaf/org",
		"leaf/stack",
		"leaf/environment",
		"leaf/tile",
		"leaf/image",
		"leaf/params",
		"leaf/volume",
		"leaf/domain",
		"leaf/domainres",
		"leaf/credential",
		"leaf/connector",
		"leaf/managed",
		"leaf/release",
		"leaf/job",
		"leaf/backup",
		"leaf/settings",
		"flow/managed",
		"leaf/domain.Syncer",
		"flow/deploy",
		"flow/promote",
		"flow/backup",
		"flow/container",
		"flow/imagewatch",
		"flow/upgrade",
		"flow/jobs",
		"flow/schedule",
		"leaf/traffic",
		"flow/traffic",
		"leaf/canvas",
		"flow/graph",
	} {
		if counts[name] == 0 {
			t.Errorf("%s never built", name)
		}
	}
	for name, n := range counts {
		if n != 1 {
			t.Errorf("%s built %d times, want 1", name, n)
		}
	}
	if err := orch.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestNewRefusesWithoutKey(t *testing.T) {
	_, err := New(Config{DataDir: t.TempDir()}, WithDocker(dockerfake.New()))
	if !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("New without key: %v, want ErrNoKey", err)
	}
}

// The installer's root domain becomes the instance domain resource on the
// first boot; a later boot adds nothing, even with another root.
func TestBootSeedsRootDomainOnce(t *testing.T) {
	dir := t.TempDir()
	boot := func(root string) []store.DomainResource {
		t.Helper()
		orch, err := New(
			Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack", RootDomain: root},
			WithDocker(dockerfake.New()),
			WithVIP(vipStub{}),
			WithProxy(func(context.Context, json.RawMessage) error { return nil }),
		)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := orch.domainres.ListAll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := orch.Close(); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	first := boot("example.com")
	if len(first) != 1 || first[0].Level != "instance" || first[0].Host != "example.com" || first[0].Declared {
		t.Fatalf("first boot = %+v, want one undeclared instance row for example.com", first)
	}
	second := boot("other.com")
	if len(second) != 1 || second[0].ID != first[0].ID || second[0].Host != "example.com" {
		t.Errorf("second boot = %+v, want the first row alone", second)
	}
}
