package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A tile has no connector field: with several shared server connectors on
// its host, its stack's config connector breaks the tie when it serves the
// host, and otherwise the error names the candidates.
func TestTileConnectorFallsBackToStack(t *testing.T) {
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
			ID:        id,
			Provider:  "github",
			Name:      name,
			Host:      host,
			Config:    `{"app":{"id":1,"slug":"` + name + `","webhook_secret":"s"}}`,
			CreatedAt: time.Now(),
			ShareAll:  true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("c1", "gh-a", "github.com")
	mk("c2", "gh-b", "github.com")
	mk("c3", "gl", "gitlab.com")
	url := "https://github.com/acme/api"

	_, err = orch.conns.For(ctx, "o1", url)
	if !errors.As(err, &connector.Ambiguous{}) || !strings.Contains(err.Error(), "gh-a") || !strings.Contains(err.Error(), "gh-b") {
		t.Errorf("ambiguous error does not name the connectors: %v", err)
	}
	for _, c := range []struct{ stack, want string }{
		{"c2", "c2"},
		{"c3", ""}, // serves another host
		{"", ""},
		{"nope", ""},
	} {
		if got := orch.tileConnector(ctx, store.Stack{OrgID: "o1", ConfigConnectorID: c.stack}, url); got != c.want {
			t.Errorf("stack connector %q: tile uses %q, want %q", c.stack, got, c.want)
		}
	}
	// one shared connector is not ambiguous: resolve by host as before
	if got := orch.tileConnector(ctx, store.Stack{OrgID: "o1", ConfigConnectorID: "c3"}, "https://gitlab.com/x/y"); got != "" {
		t.Errorf("unambiguous host named %q", got)
	}
}
