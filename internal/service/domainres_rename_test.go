package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// renameWorld is a world with the instance row, one tile with an auto host
// and a literal one.
type renameWorld struct {
	*world
	inst      DomainResource
	api       Tile
	auto, lit Domain
}

func newRenameWorld(t *testing.T) renameWorld {
	t.Helper()
	ctx := context.Background()
	w := newWorld(t)
	must(t, w.orch.domainres.SeedInstance(ctx, "example.com"))
	rows, err := w.orch.AllDomainResources(ctx)
	must(t, err)
	api := w.tile(t, "api", false)
	auto, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Auto: true})
	must(t, err)
	lit, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.shop.acme.io"})
	must(t, err)
	return renameWorld{w, rows[0], api, auto, lit}
}

func (r renameWorld) pushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pushed)
}

func (r renameWorld) hosts(t *testing.T) []string {
	t.Helper()
	ds, err := r.orch.domains.List(context.Background())
	must(t, err)
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Host + d.Path + ">" + d.RedirectTo
	}
	return out
}

func (r renameWorld) redirect(t *testing.T, host string) Domain {
	t.Helper()
	ds, err := r.orch.domains.List(context.Background())
	must(t, err)
	for _, d := range ds {
		if d.Host == host && d.RedirectTo != "" {
			return d
		}
	}
	t.Fatalf("no redirect row on %s in %v", host, r.hosts(t))
	return Domain{}
}

// Renaming the instance resource rewrites the auto host it named, leaves the
// literal one, keeps the old host as a generated redirect on the same tile,
// and pushes once with both routes.
func TestRenameDomainResource(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	pushes := w.pushCount()

	r, err := w.orch.RenameDomainResource(ctx, w.inst.ID, "New.io")
	must(t, err)
	if r.Host != "new.io" || r.ID != w.inst.ID || r.Declared != w.inst.Declared {
		t.Errorf("renamed = %+v, want the same row on new.io", r)
	}
	got, err := w.orch.domains.Get(ctx, w.auto.ID)
	must(t, err)
	if got.Host != "api.shop.acme.new.io" || !got.Auto || got.ResourceID == nil || *got.ResourceID != w.inst.ID {
		t.Errorf("auto row = %+v, want api.shop.acme.new.io named by the same resource", got)
	}
	if lit, err := w.orch.domains.Get(ctx, w.lit.ID); err != nil || lit.Host != w.lit.Host {
		t.Errorf("literal row = %+v %v, want %s kept", lit, err, w.lit.Host)
	}
	red := w.redirect(t, "api.shop.acme.example.com")
	if red.TileID != w.api.ID || red.RedirectTo != "api.shop.acme.new.io" || !red.Auto || red.ResourceID != nil {
		t.Errorf("redirect row = %+v, want a generated redirect on the same tile", red)
	}
	if n := w.pushCount(); n != pushes+1 {
		t.Errorf("pushes = %d, want exactly one", n-pushes)
	}
	push := w.lastPush()
	for _, want := range []string{"api.shop.acme.new.io", "api.shop.acme.example.com"} {
		if !strings.Contains(push, want) {
			t.Errorf("pushed config lacks %s:\n%s", want, push)
		}
	}
}

// A second rename repoints the old redirects at the newest host, so no
// chain; renaming back takes the old host over from its redirect.
func TestRenameDomainResourceChain(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	_, err := w.orch.RenameDomainResource(ctx, w.inst.ID, "new.io")
	must(t, err)
	_, err = w.orch.RenameDomainResource(ctx, w.inst.ID, "newer.io")
	must(t, err)
	if red := w.redirect(t, "api.shop.acme.example.com"); red.RedirectTo != "api.shop.acme.newer.io" {
		t.Errorf("first redirect = %+v, want it repointed at newer.io", red)
	}
	if red := w.redirect(t, "api.shop.acme.new.io"); red.RedirectTo != "api.shop.acme.newer.io" {
		t.Errorf("second redirect = %+v, want newer.io", red)
	}

	_, err = w.orch.RenameDomainResource(ctx, w.inst.ID, "example.com")
	must(t, err)
	got, err := w.orch.domains.Get(ctx, w.auto.ID)
	must(t, err)
	if got.Host != "api.shop.acme.example.com" || got.RedirectTo != "" {
		t.Errorf("after renaming back = %+v, want the real row on the old host", got)
	}
	for _, d := range w.hosts(t) {
		if strings.HasPrefix(d, "api.shop.acme.example.com>") && !strings.HasSuffix(d, ">") {
			t.Errorf("a redirect still sits on the live host: %v", w.hosts(t))
		}
	}
}

// Every refusal comes before the first write: nothing moves.
func TestRenameDomainResourceRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		host  string
		setup func(t *testing.T, w renameWorld)
		id    func(w renameWorld) string
		inval bool // an Invalid, not a Conflict
	}{
		{name: "route holds a new host", host: "new.io", setup: func(t *testing.T, w renameWorld) {
			_, err := w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "api.shop.acme.new.io", Mode: "http", Target: "10.0.0.7"})
			must(t, err)
		}},
		{name: "route holds the resource host", host: "new.io", setup: func(t *testing.T, w renameWorld) {
			_, err := w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "new.io", Mode: "http", Target: "10.0.0.7"})
			must(t, err)
		}},
		{name: "squats another org", host: "globex.io", setup: func(t *testing.T, w renameWorld) {
			must(t, w.st.Orgs.Create(ctx, store.Org{
				ID: uuid.NewString(), Name: "globex", Slug: "globex", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
			}))
		}},
		{name: "the panel host", host: "new.io", setup: func(t *testing.T, w renameWorld) {
			must(t, w.orch.SetSetting(ctx, "panel_domain", "api.shop.acme.new.io"))
		}},
		{name: "another resource holds it", host: "taken.io", setup: func(t *testing.T, w renameWorld) {
			_, err := w.orch.CreateDomainResource(ctx, domainres.Org, w.org, "taken.io", false, "")
			must(t, err)
		}},
		{name: "another tile holds a new host", host: "new.io", setup: func(t *testing.T, w renameWorld) {
			other := w.tile(t, "web", false)
			_, err := w.orch.AttachDomain(ctx, other.ID, DomainSpec{Host: "api.shop.acme.new.io"})
			must(t, err)
		}},
		{name: "a raw Caddy route", host: "new.io", setup: func(t *testing.T, w renameWorld) {
			_, err := w.orch.SetRawCaddy(ctx, w.auto.ID, `{"handle":[]}`)
			must(t, err)
		}},
		{name: "not a host", host: "new.io/x", inval: true},
		{name: "a stack resource", host: "new.io", inval: true,
			setup: func(t *testing.T, w renameWorld) {},
			id: func(w renameWorld) string {
				r, err := w.orch.CreateDomainResource(ctx, domainres.Stack, w.stack, "stack.io", false, "")
				must(t, err)
				return r.ID
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newRenameWorld(t)
			if c.setup != nil {
				c.setup(t, w)
			}
			id := w.inst.ID
			if c.id != nil {
				id = c.id(w)
			}
			before, pushes := w.hosts(t), w.pushCount()
			rows, err := w.orch.AllDomainResources(ctx)
			must(t, err)
			_, err = w.orch.RenameDomainResource(ctx, id, c.host)
			if c.inval {
				if _, ok := errs.IsInvalid(err); !ok {
					t.Fatalf("rename = %v, want an invalid input", err)
				}
			} else if _, ok := errs.IsConflict(err); !ok {
				t.Fatalf("rename = %v, want a conflict", err)
			}
			if after := w.hosts(t); strings.Join(after, "|") != strings.Join(before, "|") {
				t.Errorf("domain rows moved: %v -> %v", before, after)
			}
			after, err := w.orch.AllDomainResources(ctx)
			must(t, err)
			for i := range rows {
				if after[i].Host != rows[i].Host {
					t.Errorf("resource moved: %s -> %s", rows[i].Host, after[i].Host)
				}
			}
			if w.pushCount() != pushes {
				t.Errorf("a refused rename pushed the proxy")
			}
		})
	}
}

// An instance rename also moves the undeclared <slug>.<root> org row
// FinishOrg made, or the org's tiles would keep nesting under the old root;
// the root_domain setting follows. A declared org row is the owner's own and
// stays.
func TestRenameInstanceMovesOrgRows(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	_, err := w.orch.FinishOrg(ctx, w.org)
	must(t, err)
	own, err := w.orch.CreateDomainResource(ctx, domainres.Org, w.org, "acme.corp", false, "")
	must(t, err)
	must(t, w.orch.SetSetting(ctx, "root_domain", "example.com"))

	imp, err := w.orch.DomainResourceImpact(ctx, w.inst.ID)
	must(t, err)
	if imp.Resources != 2 {
		t.Errorf("impact = %+v, want the instance row and the org row", imp)
	}
	_, err = w.orch.RenameDomainResource(ctx, w.inst.ID, "new.io")
	must(t, err)
	rows, err := w.orch.AllDomainResources(ctx)
	must(t, err)
	hosts := map[string]bool{}
	for _, r := range rows {
		hosts[r.Host] = true
	}
	if !hosts["new.io"] || !hosts["acme.new.io"] || !hosts["acme.corp"] || hosts["acme.example.com"] {
		t.Errorf("resource hosts = %v, want new.io, acme.new.io and the declared acme.corp", hosts)
	}
	if got, _ := w.orch.Setting(ctx, "root_domain"); got != "new.io" {
		t.Errorf("root_domain = %q, want new.io", got)
	}
	if r, err := w.orch.domainres.Get(ctx, own.ID); err != nil || r.Host != "acme.corp" {
		t.Errorf("declared org row = %+v %v", r, err)
	}
}

// The impact counts what a rename moves: the rewritten hosts and one
// certificate per HTTPS host; redirects and literal hosts are not counted.
func TestDomainResourceImpact(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	got, err := w.orch.DomainResourceImpact(ctx, w.inst.ID)
	must(t, err)
	if got.Resources != 1 || got.Hosts != 1 || got.Certs != 1 {
		t.Errorf("impact = %+v, want 1 resource, 1 host, 1 certificate", got)
	}
	_, err = w.orch.RenameDomainResource(ctx, w.inst.ID, "new.io")
	must(t, err)
	got, err = w.orch.DomainResourceImpact(ctx, w.inst.ID)
	must(t, err)
	if got.Hosts != 1 {
		t.Errorf("impact after = %+v, the redirect row must not count", got)
	}
}

// root_domain renames the instance row; a later boot with the old root in
// its config does not seed it back; refusals write nothing.
func TestSetRootDomainRenames(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	must(t, w.orch.SetSetting(ctx, "root_domain", "example.com"))
	must(t, w.orch.SetSetting(ctx, "root_domain", "*.new.io"))
	rows, err := w.orch.AllDomainResources(ctx)
	must(t, err)
	if len(rows) != 1 || rows[0].ID != w.inst.ID || rows[0].Host != "new.io" {
		t.Fatalf("rows = %+v, want the instance row renamed", rows)
	}
	if got, _ := w.orch.Setting(ctx, "root_domain"); got != "*.new.io" {
		t.Errorf("root_domain = %q", got)
	}
	if got, err := w.orch.domains.Get(ctx, w.auto.ID); err != nil || got.Host != "api.shop.acme.new.io" {
		t.Errorf("auto host = %+v %v", got, err)
	}

	err = w.orch.SetSettings(ctx, map[string]string{"root_domain": "taken.io", "panel_domain": "x.example.org"})
	must(t, err)
	_, err = w.orch.CreateDomainResource(ctx, domainres.Org, w.org, "other.io", false, "")
	must(t, err)
	if err := w.orch.SetSetting(ctx, "root_domain", "other.io"); err == nil {
		t.Error("root_domain onto another resource's host was accepted")
	}
	if got, _ := w.orch.Setting(ctx, "root_domain"); got != "taken.io" {
		t.Errorf("root_domain after a refusal = %q, want taken.io", got)
	}
	if err := w.orch.SetSetting(ctx, "root_domain", "10.0.0.1"); err == nil {
		t.Error("an address as root_domain was accepted")
	}
	if err := w.orch.SetSetting(ctx, "root_domain", ""); err == nil {
		t.Error("an empty root_domain was accepted over a live one")
	}
}

// After the setting moved the instance row, a boot whose config still names
// the old root adds nothing: the row exists.
func TestBootAfterRootRename(t *testing.T) {
	dir := t.TempDir()
	boot := func() (*Orchestrator, []store.DomainResource) {
		t.Helper()
		orch, err := New(
			Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack", RootDomain: "example.com"},
			WithDocker(dockerfake.New()),
			WithVIP(vipStub{}),
			WithProxy(func(context.Context, json.RawMessage) error { return nil }),
		)
		must(t, err)
		rows, err := orch.domainres.ListAll(context.Background())
		must(t, err)
		return orch, rows
	}
	orch, first := boot()
	if len(first) != 1 || first[0].Host != "example.com" {
		t.Fatalf("first boot = %+v", first)
	}
	must(t, orch.SetSetting(context.Background(), "root_domain", "new.io"))
	must(t, orch.Close())
	orch, second := boot()
	defer func() { must(t, orch.Close()) }()
	if len(second) != 1 || second[0].ID != first[0].ID || second[0].Host != "new.io" {
		t.Errorf("second boot = %+v, want the one row on new.io", second)
	}
}

// A stack rename recomputes auto hosts; a generated redirect is auto too but
// is not recomputed: it follows the row it was made for.
func TestRenameStackRepointsGeneratedRedirects(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	_, err := w.orch.RenameDomainResource(ctx, w.inst.ID, "new.io")
	must(t, err)
	_, err = w.orch.RenameStack(ctx, w.stack, "store")
	must(t, err)
	if red := w.redirect(t, "api.shop.acme.example.com"); red.RedirectTo != "api.store.acme.new.io" {
		t.Errorf("redirect = %+v, want it repointed at the row's new host", red)
	}
	if got, err := w.orch.domains.Get(ctx, w.auto.ID); err != nil || got.Host != "api.store.acme.new.io" {
		t.Errorf("auto row = %+v %v, want api.store.acme.new.io", got, err)
	}
}

// F4: another org's generated redirect is theirs. Org beta renames bcorp.io
// away, leaving a redirect on api.shop.bcorp.io (beta's tile); org acme
// renaming its own resource onto bcorp.io must not take that host over.
func TestRenameDoesNotReclaimAnotherOrgsRedirect(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	now := time.Now()
	org, stack, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	must(t, w.st.Orgs.Create(ctx, store.Org{ID: org, Name: "beta", Slug: "beta", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, w.st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: env, StackID: stack, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "nb", FromKind: "branch", FromBranch: "main", CreatedAt: now,
	}))
	bt, err := w.orch.CreateTile(ctx, Tile{StackID: stack, EnvironmentID: env, Name: "api", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	bres, err := w.orch.CreateDomainResource(ctx, domainres.Org, org, "bcorp.io", false, "")
	must(t, err)
	_, err = w.orch.AttachDomain(ctx, bt.ID, DomainSpec{Auto: true})
	must(t, err)
	_, err = w.orch.RenameDomainResource(ctx, bres.ID, "bcorp.com")
	must(t, err)

	ares, err := w.orch.CreateDomainResource(ctx, domainres.Org, w.org, "acorp.io", false, "")
	must(t, err)
	// Org acme's api tile takes the nearest resource: its own acorp.io.
	ad, err := w.orch.AttachDomain(ctx, w.api.ID, DomainSpec{Auto: true})
	must(t, err)
	if ad.Host != "api.shop.acorp.io" {
		t.Fatalf("acme auto host = %s, want api.shop.acorp.io", ad.Host)
	}
	_, err = w.orch.RenameDomainResource(ctx, ares.ID, "bcorp.io")
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("rename onto another org's redirect = %v, want a conflict", err)
	}
	red := w.redirect(t, "api.shop.bcorp.io")
	if red.TileID != bt.ID {
		t.Errorf("redirect = %+v, want it still on beta's tile", red)
	}
	if got, err := w.orch.domains.Get(ctx, ad.ID); err != nil || got.Host != "api.shop.acorp.io" {
		t.Errorf("acme row = %+v %v, want it unmoved", got, err)
	}
}

// F4: root_domain stores the cleaned host, so the setting and the instance
// row stay in step and a later change finds the row.
func TestSetRootDomainStoresCleanedHost(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	must(t, w.orch.SetSetting(ctx, "root_domain", "example.com"))
	must(t, w.orch.SetSetting(ctx, "root_domain", "https://New.io/"))
	if got, _ := w.orch.Setting(ctx, "root_domain"); got != "new.io" {
		t.Fatalf("root_domain = %q, want new.io", got)
	}
	must(t, w.orch.SetSetting(ctx, "root_domain", "other.io"))
	rows, err := w.orch.AllDomainResources(ctx)
	must(t, err)
	if len(rows) != 1 || rows[0].Host != "other.io" {
		t.Errorf("rows = %+v, want the instance row on other.io", rows)
	}
	if got, err := w.orch.domains.Get(ctx, w.auto.ID); err != nil || got.Host != "api.shop.acme.other.io" {
		t.Errorf("auto host = %+v %v", got, err)
	}
}

// F4: a root_domain that names no instance row while another one exists is
// refused, not stored beside a row that stays put.
func TestSetRootDomainRefusesUnmatchedRow(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	must(t, w.orch.SetSetting(ctx, "root_domain", "example.com"))
	must(t, w.orch.settings.Set(ctx, "root_domain", "gone.io")) // desynced by hand
	if err := w.orch.SetSetting(ctx, "root_domain", "other.io"); err == nil {
		t.Fatal("root_domain moved with no instance row to follow")
	}
	if got, _ := w.orch.Setting(ctx, "root_domain"); got != "gone.io" {
		t.Errorf("root_domain = %q after a refusal, want gone.io", got)
	}
}

// F4: the generated redirect keeps the moved row's HTTPS flags.
func TestRenameRedirectKeepsHTTPSFlags(t *testing.T) {
	w := newRenameWorld(t)
	ctx := context.Background()
	off := false
	s, err := specOf(w.auto)
	must(t, err)
	s.HTTPS, s.ForceHTTPS = &off, &off
	_, err = w.orch.domains.Update(ctx, w.auto, s, false)
	must(t, err)
	_, err = w.orch.RenameDomainResource(ctx, w.inst.ID, "new.io")
	must(t, err)
	if red := w.redirect(t, "api.shop.acme.example.com"); red.HTTPS || red.ForceHTTPS {
		t.Errorf("redirect = %+v, want https and force off like the moved row", red)
	}
}
