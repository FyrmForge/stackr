package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// panel_domain is stored in the form it was checked in: a pasted URL, a
// host:port or a trailing dot would match no request at the proxy. A
// wildcard, localhost and a bare address are refused.
func TestPanelDomainStoredClean(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for in, want := range map[string]string{
		"https://New.Example.com": "new.example.com",
		"new.example.com:443":     "new.example.com",
		"new.example.com.":        "new.example.com",
	} {
		must(t, w.orch.SetSetting(ctx, "panel_domain", in))
		if got, _ := w.orch.Setting(ctx, "panel_domain"); got != want {
			t.Errorf("panel_domain %q stored as %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"*.example.com", "localhost", "10.0.0.1"} {
		err := w.orch.SetSetting(ctx, "panel_domain", bad)
		if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("panel_domain %q = %v, want Invalid", bad, err)
		}
	}
	if got, _ := w.orch.Setting(ctx, "panel_domain"); got != "new.example.com" {
		t.Errorf("panel_domain = %q after the refusals", got)
	}
	must(t, w.orch.SetSetting(ctx, "panel_domain", ""))
}

// root_domain and panel_domain in one save: the panel may not land on a
// host the root rename creates, and the refusal writes neither.
func TestSetSettingsRootAndPanelTogether(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	must(t, w.orch.SetSetting(ctx, "root_domain", "example.com"))
	for range 10 { // the old code wrote in map order
		err := w.orch.SetSettings(ctx, map[string]string{"root_domain": "new.io", "panel_domain": "api.shop.acme.new.io"})
		if !isConflictErr(err) {
			t.Fatalf("panel on a renamed tile host = %v, want Conflict", err)
		}
		if r, _ := w.orch.Setting(ctx, "root_domain"); r != "example.com" {
			t.Fatalf("root_domain = %q after a refused save", r)
		}
		if p, _ := w.orch.Setting(ctx, "panel_domain"); p != "" {
			t.Fatalf("panel_domain = %q after a refused save", p)
		}
	}
	// a panel that does not collide lands with the root
	must(t, w.orch.SetSettings(ctx, map[string]string{"root_domain": "new.io", "panel_domain": "panel.new.io"}))
	if r, _ := w.orch.Setting(ctx, "root_domain"); r != "new.io" {
		t.Errorf("root_domain = %q", r)
	}
	if p, _ := w.orch.Setting(ctx, "panel_domain"); p != "panel.new.io" {
		t.Errorf("panel_domain = %q", p)
	}
}

func isConflictErr(err error) bool { _, ok := errs.IsConflict(err); return ok }

// newPickyWorld is a world whose proxy refuses any config holding BADROUTE,
// as Caddy refuses a route it cannot load.
func newPickyWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{fake: dockerfake.New()}
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(w.fake),
		WithVIP(vipStub{}),
		WithProxy(func(_ context.Context, cfg json.RawMessage) error {
			if strings.Contains(string(cfg), "BADROUTE") {
				return errors.New("caddy: cannot load")
			}
			return nil
		}),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	w.orch = orch
	return w
}

func customRoute(host string) string {
	return `[{"match":[{"host":["` + host + `"]}],"handle":[{"handler":"static_response","body":"hi"}]}]`
}

// Two saves of proxy_custom at once, one Caddy cannot load: the take-back of
// the bad one must not undo the good one.
func TestProxyCustomTakeBackRace(t *testing.T) {
	ctx := context.Background()
	for i := range 40 {
		w := newPickyWorld(t)
		must(t, w.orch.SetSetting(ctx, "proxy_custom", customRoute("old.example.com")))
		good := customRoute("good.example.com")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = w.orch.SetSetting(ctx, "proxy_custom", customRoute("BADROUTE.example.com"))
		}()
		go func() { defer wg.Done(); _ = w.orch.SetSetting(ctx, "proxy_custom", good) }()
		wg.Wait()
		if got, _ := w.orch.Setting(ctx, "proxy_custom"); got != good {
			t.Fatalf("round %d: proxy_custom = %s, want the good save to survive", i, got)
		}
	}
}

// A share row for an org slugged "all" grants that one org; only the
// Field "share" row turns share-all on.
func TestApplySharesFieldNotValue(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	conn := uuid.NewString()
	must(t, w.st.Connectors.Create(ctx, store.Connector{
		ID: conn, Provider: "github", Name: "gh", Host: "github.com",
		Config: `{"app":{"id":2,"slug":"stackr-gh","webhook_secret":"s"}}`, CreatedAt: time.Now(),
	}))
	allOrg := uuid.NewString()
	must(t, w.st.Orgs.Create(ctx, store.Org{ID: allOrg, Name: "all", Slug: "all", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}))
	live := serverconfig.Live{Connectors: []serverconfig.ConnectorLive{{Connector: store.Connector{ID: conn, Name: "gh"}}}}
	wk := &serverWalk{o: w.orch, ctx: ctx, live: live, log: io.Discard, orgIDs: map[string]string{"all": allOrg}}

	later, err := wk.applyShares([]serverconfig.Change{{Kind: "connector-share", Tile: "gh", Field: "org", New: "all"}})
	must(t, err)
	c, err := w.orch.conns.Server(ctx, conn)
	must(t, err)
	if len(later) != 0 || c.ShareAll {
		t.Fatalf("org row named all: later = %v, share_all = %v; want that one org shared", later, c.ShareAll)
	}
	if got, _ := w.orch.conns.SharedOrgs(ctx, conn); len(got) != 1 || got[0] != allOrg {
		t.Errorf("shared orgs = %v, want [%s]", got, allOrg)
	}
	_, err = wk.applyShares([]serverconfig.Change{{Kind: "connector-share", Tile: "gh", Field: "share", New: "all"}})
	must(t, err)
	if c, _ = w.orch.conns.Server(ctx, conn); !c.ShareAll {
		t.Error("the share-all row did not turn share-all on")
	}
}
