package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func newPinRig(t *testing.T) (*Orchestrator, store.Stack, store.Stack) {
	t.Helper()
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
		ID: "o1", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
		ConfigRepo: "https://github.com/acme/cfg",
	}); err != nil {
		t.Fatal(err)
	}
	org := "o1"
	mk := func(id, host string, own bool) {
		c := store.Connector{
			ID: id, Kind: "github", Capabilities: store.StringList{"git"}, Name: id, Host: host,
			Settings: `{"app":{"id":1,"slug":"` + id + `","webhook_secret":"s"}}`, CreatedAt: time.Now(),
		}
		if own {
			c.OrgID = &org
		} else {
			c.ShareAll = true
		}
		if err := orch.store.Connectors.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	mk("c1", "github.com", true)
	mk("g1", "gitlab.com", false)
	mk("g2", "gitlab.com", false)
	stack := func(slug, repo string) store.Stack {
		st, err := orch.stacks.Create(ctx, "o1", slug, "")
		if err != nil {
			t.Fatal(err)
		}
		st.ConfigRepo = repo
		if err := orch.store.Stacks.Update(ctx, st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	// the clean install already ran the pin on an empty database: run it again
	if err := orch.store.Settings.Delete(ctx, keyConnectorPin); err != nil {
		t.Fatal(err)
	}
	return orch, stack("gh", "https://github.com/acme/shop"), stack("gl", "https://gitlab.com/acme/shop")
}

// The pin writes today's host pick into empty connector ids of a bound org
// and its stacks, leaves an ambiguous host empty, and runs once.
func TestPinConnectors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orch, gh, gl := newPinRig(t)
	conn := func(id string) string {
		st, err := orch.store.Stacks.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return st.ConfigConnectorID
	}
	orch.pinConnectors(ctx)
	if og, _ := orch.orgs.Get(ctx, "o1"); og.ConfigConnectorID != "c1" {
		t.Errorf("org pin = %q", og.ConfigConnectorID)
	}
	if got := conn(gh.ID); got != "c1" {
		t.Errorf("stack pin = %q", got)
	}
	if got := conn(gl.ID); got != "" {
		t.Errorf("ambiguous host pinned %q", got)
	}
	// a plan sets a stack to inherit; a restart must not pin it again
	gh.ConfigConnectorID = ""
	if err := orch.store.Stacks.Update(ctx, gh); err != nil {
		t.Fatal(err)
	}
	orch.pinConnectors(ctx)
	if got := conn(gh.ID); got != "" {
		t.Errorf("pin ran twice: %q", got)
	}
}

// A repo the connector in use does not list is warned about in the org plan
// notes, the stack plan warnings and Used by; a list not known warns nothing.
func TestRepoWarning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orch, gh, _ := newPinRig(t)
	orch.pinConnectors(ctx)
	c, err := orch.store.Connectors.Get(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if w := orch.repoWarning(c, gh.ConfigRepo); w != "" {
		t.Errorf("list not known warned: %q", w)
	}
	orch.conns.NoteRepos("c1", []connectors.Repo{{FullName: "acme/other", URL: "https://github.com/acme/other"}}, nil)
	w := orch.repoWarning(c, gh.ConfigRepo)
	if !strings.Contains(w, "connector c1 does not list acme/shop") {
		t.Errorf("warning = %q", w)
	}
	if w := orch.repoWarningFor(ctx, gh, gh.ConfigRepo, false); w == "" {
		t.Error("stack plan seam gave no warning")
	}
	if w := orch.repoWarningFor(ctx, gh, "https://github.com/acme/api", true); w == "" {
		t.Error("tile seam gave no warning")
	}
	og, _ := orch.orgs.Get(ctx, "o1")
	conns, _ := orch.conns.ListConnected(ctx, "o1")
	live := orgconfig.Live{Org: og, Connectors: conns}
	orch.orgFileWarns(ctx, &live, &orgconfig.File{Stacks: map[string]orgconfig.StackRef{
		"gh": {Repo: gh.ConfigRepo},
		"ok": {Repo: "https://gitlab.com/acme/x", Connector: "g1"},
	}})
	if len(live.FileNotes) != 1 || !strings.HasPrefix(live.FileNotes[0], "stacks.gh: connector c1 does not list acme/shop") {
		t.Errorf("org plan notes = %q", live.FileNotes)
	}
	us, err := orch.ConnectorUsages(ctx, nil, "c1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range us {
		found = found || strings.HasSuffix(u.What, NotListed)
	}
	if !found {
		t.Errorf("Used by lacks the note: %+v", us)
	}
	orch.conns.NoteRepos("c1", []connectors.Repo{{FullName: "acme/shop", URL: "https://github.com/acme/shop.git"}}, nil)
	if w := orch.repoWarning(c, gh.ConfigRepo); w != "" {
		t.Errorf("listed repo warned: %q", w)
	}
	orch.conns.NoteRepos("c1", nil, context.Canceled)
	if w := orch.repoWarning(c, gh.ConfigRepo); w != "" {
		t.Errorf("failed list warned: %q", w)
	}
}

// An org with no config repo gets the pick of the one host its stacks use; two
// hosts leave it empty.
func TestPinConnectorsRepolessOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orch, _, _ := newPinRig(t)
	for _, id := range []string{"o2", "o3"} {
		if err := orch.store.Orgs.Create(ctx, store.Org{
			ID: id, Name: id, Slug: id, EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := orch.store.Connectors.Create(ctx, store.Connector{
			ID: "gh-" + id, Kind: "github", Capabilities: store.StringList{"git"}, Name: id, Host: "github.com",
			OrgID: new(id), Settings: `{"app":{"id":1,"slug":"x","webhook_secret":"s"}}`, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	stack := func(org, slug, repo string) {
		st, err := orch.stacks.Create(ctx, org, slug, "")
		if err != nil {
			t.Fatal(err)
		}
		st.ConfigRepo = repo
		if err := orch.store.Stacks.Update(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	stack("o2", "a", "https://github.com/o2/a")
	stack("o3", "a", "https://github.com/o3/a")
	stack("o3", "b", "https://gitlab.com/o3/b")
	if err := orch.store.Settings.Delete(ctx, keyConnectorPin); err != nil {
		t.Fatal(err)
	}
	orch.pinConnectors(ctx)
	for id, want := range map[string]string{"o2": "gh-o2", "o3": ""} {
		if og, _ := orch.orgs.Get(ctx, id); og.ConfigConnectorID != want {
			t.Errorf("org %s pin = %q, want %q", id, og.ConfigConnectorID, want)
		}
	}
}

// A repo-less org whose tiles span two hosts is left unpinned; the tiles on
// each host that now have no connector are counted in a problem the admin
// sees, and setting the org's connector clears it.
func TestPinConnectorsTwoHostsProblem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orch, _, _ := newPinRig(t)
	org := "o5"
	if err := orch.store.Orgs.Create(ctx, store.Org{
		ID: org, Name: "five", Slug: "five", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for id, host := range map[string]string{"h-gh": "github.com", "h-bb": "bitbucket.org"} {
		c := store.Connector{
			ID: id, Kind: "github", Capabilities: store.StringList{"git"}, Name: id, Host: host,
			Settings: `{"app":{"id":1,"slug":"x","webhook_secret":"s"}}`, CreatedAt: time.Now(),
		}
		if id == "h-gh" {
			c.OrgID = &org
		} else {
			c.ShareAll = true
		}
		if err := orch.store.Connectors.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	st, err := orch.stacks.Create(ctx, org, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	env := store.Environment{ID: "e5", StackID: st.ID, Name: "Dev", Slug: "dev", Type: "static", Settings: "{}", Network: "n", FromKind: "branch", CreatedAt: time.Now()}
	if err := orch.store.Environments.Create(ctx, env); err != nil {
		t.Fatal(err)
	}
	for id, url := range map[string]string{"t1": "https://github.com/five/a", "t2": "https://bitbucket.org/five/b"} {
		if err := orch.store.Tiles.Create(ctx, store.Tile{
			ID: id, StackID: st.ID, EnvironmentID: env.ID, Name: id, Slug: id, Kind: "service", GitURL: url,
			EnvJSON: "{}", BuildArgs: "{}", Replicas: 1, UpdatePolicy: "manual", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := orch.store.Settings.Delete(ctx, keyConnectorPin); err != nil {
		t.Fatal(err)
	}
	orch.pinConnectors(ctx)
	if og, _ := orch.orgs.Get(ctx, org); og.ConfigConnectorID != "" {
		t.Errorf("two hosts were pinned to %q", og.ConfigConnectorID)
	}
	ps, err := orch.Problems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, p := range ps {
		if p.Key == "config.connector."+org {
			got = p.Message
		}
	}
	if !strings.Contains(got, "2 tiles") || !strings.Contains(got, "no connector") {
		t.Fatalf("problem = %q", got)
	}
	if _, err := orch.SetOrgConnector(ctx, org, "h-gh"); err != nil {
		t.Fatal(err)
	}
	if orch.hasProblem(ctx, "alert.config.connector."+org) {
		t.Error("the problem stayed after the org got a connector")
	}
}
