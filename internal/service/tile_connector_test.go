package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A tile has no connector field: it clones through its stack's connector
// when that serves the host, else the org's; never by host alone.
func TestTileConnectorFallsBackToStack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
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
	if err := orch.store.Orgs.Create(ctx, store.Org{
		ID:        "o1",
		Name:      "acme",
		Slug:      "acme",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, name, host string) {
		t.Helper()
		if err := orch.store.Connectors.Create(ctx, store.Connector{
			ID:           id,
			Kind:         "github",
			Capabilities: store.StringList{"git"},
			Name:         name,
			Host:         host,
			Settings:     `{"app":{"id":1,"slug":"` + name + `","webhook_secret":"s"}}`,
			CreatedAt:    time.Now(),
			ShareAll:     true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("c1", "gh-a", "github.com")
	mk("c2", "gh-b", "github.com")
	mk("c3", "gl", "gitlab.com")
	url := "https://github.com/acme/api"
	for _, c := range []struct{ stack, want string }{
		{"c2", "c2"},
		{"c3", ""}, // serves another host
		{"", ""},   // two on the host and nothing named: no host pick
		{"nope", ""},
	} {
		if got := orch.tileConnector(ctx, store.Stack{OrgID: "o1", ConfigConnectorID: c.stack}, url); got != c.want {
			t.Errorf("stack connector %q: tile uses %q, want %q", c.stack, got, c.want)
		}
	}
	// a stack that names none inherits the org's connector
	og, _ := orch.orgs.Get(ctx, "o1")
	if _, err := orch.orgs.SetConfigRepo(ctx, og, "c1", "https://github.com/acme/cfg", "", "", false); err != nil {
		t.Fatal(err)
	}
	if got := orch.tileConnector(ctx, store.Stack{OrgID: "o1"}, url); got != "c1" {
		t.Errorf("tile inherits org: %q", got)
	}
	if got := orch.tileConnector(ctx, store.Stack{OrgID: "o1", ConfigConnectorID: "c2"}, url); got != "c2" {
		t.Errorf("stack beats org: %q", got)
	}
	// clones without a connector are refused with NoConnector
	if _, err := orch.cloneEnv(ctx, "o1", "", url); !errors.As(err, &connector.NoConnector{}) {
		t.Errorf("cloneEnv with no connector: %v", err)
	}
}

// An org with no config repo and its own connector: a tile that names none
// clones through it.
func TestTileInRepolessOrgUsesOrgConnector(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orch, _, _ := newPinRig(t)
	if err := orch.store.Orgs.Create(ctx, store.Org{
		ID: "o2", Name: "solo", Slug: "solo", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := orch.store.Connectors.Create(ctx, store.Connector{
		ID: "own", Kind: "github", Capabilities: store.StringList{"git"}, Name: "own", Host: "github.com",
		OrgID: new("o2"), Settings: `{"app":{"id":1,"slug":"own","webhook_secret":"s"}}`, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/solo/api"
	if got := orch.tileConnector(ctx, store.Stack{OrgID: "o2"}, url); got != "" {
		t.Errorf("before the pick the tile uses %q", got)
	}
	og, _ := orch.orgs.Get(ctx, "o2")
	if _, err := orch.orgs.SetConnector(ctx, og, "own"); err != nil {
		t.Fatal(err)
	}
	if got := orch.tileConnector(ctx, store.Stack{OrgID: "o2"}, url); got != "own" {
		t.Errorf("tile uses %q, want the org's", got)
	}
}
