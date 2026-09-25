package domainres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// world is orgs acme (o1) and globex (o2), stack shop (s1) in acme and
// stack mart (s2) in globex.
func world(t *testing.T) (*store.Store, *domainres.Leaf, []store.Org) {
	t.Helper()
	st := servicetest.Store(t)
	orgs := []store.Org{
		{ID: "o1", Name: "Acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: t0},
		{ID: "o2", Name: "Globex", Slug: "globex", EnvColors: "{}", Settings: "{}", CreatedAt: t0},
	}
	for _, o := range orgs {
		must(t, st.Orgs.Create(ctx, o))
	}
	for _, s := range []store.Stack{
		{ID: "s1", OrgID: "o1", Name: "Shop", Slug: "shop", Settings: "{}", CreatedAt: t0},
		{ID: "s2", OrgID: "o2", Name: "Mart", Slug: "mart", Settings: "{}", CreatedAt: t0},
	} {
		must(t, st.Stacks.Create(ctx, s))
	}
	return st, domainres.New(st.DomainResources), orgs
}

func invalid(t *testing.T, err error, field string) {
	t.Helper()
	v, ok := errs.IsInvalid(err)
	if !ok || v.Field != field {
		t.Errorf("err = %v, want Invalid on %s", err, field)
	}
}

func conflict(t *testing.T, err error, msg string) {
	t.Helper()
	c, ok := errs.IsConflict(err)
	if !ok || c.Msg != msg {
		t.Errorf("err = %v, want Conflict %q", err, msg)
	}
}

func TestCreateGrammar(t *testing.T) {
	_, l, orgs := world(t)
	for _, host := range []string{
		"",
		"https://shop.io",
		"shop.io/a",
		"shop.io:443",
		"a b.io",
		"*.shop.io",
		"localhost",
		"10.0.0.1",
		"shop",
		"-shop.io",
		"shop.123",
		"sh_op.io",
	} {
		_, err := l.Create(ctx, domainres.Spec{Level: domainres.Instance, Host: host}, "", orgs)
		invalid(t, err, "host")
	}

	_, err := l.Create(ctx, domainres.Spec{Level: "node", Host: "shop.io"}, "", orgs)
	invalid(t, err, "level")
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Org, Host: "shop.io"}, "", orgs)
	invalid(t, err, "owner")
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Instance, Host: "shop.io", ACMEEmail: "Ops <ops@shop.io>"}, "", orgs)
	invalid(t, err, "acme_email")

	r, err := l.Create(ctx, domainres.Spec{
		Level:               domainres.Stack,
		OwnerID:             "s1",
		Host:                " Shop.IO. ",
		IncludeEnvOnDefault: true,
		ACMEEmail:           " Ops@Shop.io ",
	}, "o1", orgs)
	must(t, err)
	got, err := l.Get(ctx, r.ID)
	must(t, err)
	if got.Host != "shop.io" || got.ACMEEmail != "ops@shop.io" || !got.Declared || !got.IncludeEnvOnDefault ||
		got.Level != domainres.Stack || got.StackID == nil || *got.StackID != "s1" || got.OrgID != nil {
		t.Errorf("stored %+v", got)
	}
}

func TestCreateTaken(t *testing.T) {
	_, l, orgs := world(t)
	_, err := l.Create(ctx, domainres.Spec{Level: domainres.Org, OwnerID: "o1", Host: "shop.io"}, "o1", orgs)
	must(t, err)
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Stack, OwnerID: "s2", Host: "SHOP.io"}, "o2", orgs)
	conflict(t, err, "shop.io is already a domain resource.")
	all, err := l.ListAll(ctx)
	must(t, err)
	if len(all) != 1 {
		t.Errorf("rows = %d, want 1", len(all))
	}
}

// Both directions on one label rule: a resource may not lead with another
// org's slug, and an org may not rename onto the slug a foreign resource
// leads with. An org's own rows, and its stacks' rows, never count.
func TestSquatBothWays(t *testing.T) {
	st, l, orgs := world(t)

	_, err := l.Create(ctx, domainres.Spec{Level: domainres.Org, OwnerID: "o1", Host: "globex.example.com"}, "o1", orgs)
	conflict(t, err, "That hostname starts with another organization's slug.")
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Instance, Host: "acme.example.net"}, "", orgs)
	conflict(t, err, "That hostname starts with another organization's slug.")
	if err := domainres.CheckOrgSquat("*.globex.io", "o1", orgs); err == nil {
		t.Error("a wildcard leading with another org's slug passed")
	}
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Org, OwnerID: "o1", Host: "acme.example.com"}, "o1", orgs)
	must(t, err)
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Stack, OwnerID: "s1", Host: "acme.shop.io"}, "o1", orgs)
	must(t, err)
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Org, OwnerID: "o1", Host: "initech.example.com"}, "o1", orgs)
	must(t, err)
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Stack, OwnerID: "s1", Host: "umbrella.shop.io"}, "o1", orgs)
	must(t, err)

	// Reverse: the claims the orchestrator builds, a stack row counted
	// against its stack's org.
	all, err := l.ListAll(ctx)
	must(t, err)
	stackOrg := map[string]string{"s1": "o1", "s2": "o2"}
	var claims []org.Claim
	for _, r := range all {
		c := org.Claim{Host: r.Host}
		switch {
		case r.OrgID != nil:
			c.OrgID = *r.OrgID
		case r.StackID != nil:
			c.OrgID = stackOrg[*r.StackID]
		}
		claims = append(claims, c)
	}
	ol := org.New(st.Orgs, st.OrgMembers, st.Invites)
	_, err = ol.Rename(ctx, orgs[1], "Initech", claims)
	invalid(t, err, "name")
	_, err = ol.Rename(ctx, orgs[1], "Umbrella", claims)
	invalid(t, err, "name")
	acme, err := ol.Rename(ctx, orgs[0], "Initech", claims)
	must(t, err)
	_, err = ol.Rename(ctx, acme, "Umbrella", claims)
	must(t, err)
}

func TestVisibleOrder(t *testing.T) {
	st, l, _ := world(t)
	ptr := func(s string) *string { return &s }
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	for _, r := range []store.DomainResource{
		{ID: "i1", Level: "instance", Host: "example.com", CreatedAt: at(0)},
		{ID: "i2", Level: "instance", Host: "example.net", Declared: true, CreatedAt: at(2)},
		{ID: "oa", Level: "org", OrgID: ptr("o1"), Host: "a.acme.io", Declared: true, CreatedAt: at(3)},
		{ID: "ob", Level: "org", OrgID: ptr("o1"), Host: "b.acme.io", CreatedAt: at(0)},
		{ID: "oc", Level: "org", OrgID: ptr("o1"), Host: "c.acme.io", Declared: true, CreatedAt: at(1)},
		{ID: "og", Level: "org", OrgID: ptr("o2"), Host: "globex.io", Declared: true, CreatedAt: at(0)},
		{ID: "s1", Level: "stack", StackID: ptr("s1"), Host: "shop.io", Declared: true, CreatedAt: at(5)},
		{ID: "s2", Level: "stack", StackID: ptr("s2"), Host: "mart.io", Declared: true, CreatedAt: at(0)},
	} {
		must(t, st.DomainResources.Create(ctx, r))
	}
	got, err := l.Visible(ctx, "s1", "o1")
	must(t, err)
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	want := []string{"s1", "oc", "oa", "ob", "i2", "i1"}
	if len(ids) != len(want) {
		t.Fatalf("visible = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("visible = %v, want %v", ids, want)
		}
	}
}

func TestAutoHost(t *testing.T) {
	instance := store.DomainResource{Level: "instance", Host: "example.com"}
	orgRow := store.DomainResource{Level: "org", Host: "acme.io"}
	stackRow := store.DomainResource{Level: "stack", Host: "shop.io"}
	withEnv := func(r store.DomainResource) store.DomainResource {
		r.IncludeEnvOnDefault = true
		return r
	}
	for _, c := range []struct {
		res       store.DomainResource
		env       string
		isDefault bool
		want      string
	}{
		{instance, "dev", false, "api.dev.shop.acme.example.com"},
		{instance, "prod", true, "api.shop.acme.example.com"},
		{withEnv(instance), "prod", true, "api.prod.shop.acme.example.com"},
		{orgRow, "dev", false, "api.dev.shop.acme.io"},
		{orgRow, "prod", true, "api.shop.acme.io"},
		{withEnv(orgRow), "prod", true, "api.prod.shop.acme.io"},
		{stackRow, "dev", false, "api.dev.shop.io"},
		{stackRow, "prod", true, "api.shop.io"},
		{withEnv(stackRow), "prod", true, "api.prod.shop.io"},
		{withEnv(stackRow), "dev", false, "api.dev.shop.io"},
	} {
		got := domainres.AutoHost(c.res, "acme", "shop", c.env, "api", c.isDefault)
		if got != c.want {
			t.Errorf("AutoHost(%s %s, %s, default=%v) = %s, want %s", c.res.Level, c.res.Host, c.env, c.isDefault, got, c.want)
		}
	}
}

// Refused with a count while tile domains name it; the foreign key stops a
// caller that miscounts.
func TestDelete(t *testing.T) {
	e := servicetest.New(t)
	l := domainres.New(e.Store.DomainResources)
	orgID := e.Org(t, "acme")
	tl := e.Tile(t, orgID)
	r, err := l.Create(ctx, domainres.Spec{Level: domainres.Stack, OwnerID: tl.Stack, Host: "shop.io"}, orgID, nil)
	must(t, err)

	conflict(t, l.Delete(ctx, r, 1), "1 tile domain is named by this resource.")
	conflict(t, l.Delete(ctx, r, 3), "3 tile domains are named by this resource.")

	must(t, e.Store.Domains.Create(ctx, store.Domain{
		ID:         "d1",
		TileID:     tl.ID,
		Host:       "api.shop.io",
		Path:       "/",
		Auto:       true,
		ResourceID: &r.ID,
		ProxyJSON:  "{}",
		CreatedAt:  t0,
	}))
	if err := l.Delete(ctx, r, 0); err == nil {
		t.Fatal("deleted a resource a tile domain names")
	}

	must(t, e.Store.Domains.Delete(ctx, "d1"))
	must(t, l.Delete(ctx, r, 0))
	if _, err := l.Get(ctx, r.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
}

func TestUpdate(t *testing.T) {
	_, l, orgs := world(t)
	r, err := l.Create(ctx, domainres.Spec{Level: domainres.Org, OwnerID: "o1", Host: "acme.io"}, "o1", orgs)
	must(t, err)
	_, err = l.Update(ctx, r, true, "not an email")
	invalid(t, err, "acme_email")
	r, err = l.Update(ctx, r, true, " Ops@Acme.io ")
	must(t, err)
	got, err := l.Get(ctx, r.ID)
	must(t, err)
	if got.ACMEEmail != "ops@acme.io" || !got.IncludeEnvOnDefault {
		t.Errorf("updated = %+v", got)
	}
	_, err = l.Update(ctx, got, false, "")
	must(t, err)
	got, err = l.Get(ctx, r.ID)
	must(t, err)
	if got.ACMEEmail != "" || got.IncludeEnvOnDefault {
		t.Errorf("cleared = %+v", got)
	}
}

// A plan's pending rows stand in for stored ones by id, or join the list.
func TestVisiblePending(t *testing.T) {
	_, l, orgs := world(t)
	r, err := l.Create(ctx, domainres.Spec{Level: domainres.Org, OwnerID: "o1", Host: "acme.io"}, "o1", orgs)
	must(t, err)
	r.IncludeEnvOnDefault = true
	add, err := domainres.Prepare(domainres.Spec{ID: "new", Level: domainres.Stack, OwnerID: "s1", Host: "shop.io"}, "o1", orgs)
	must(t, err)
	got, err := l.Visible(ctx, "s1", "o1", r, add)
	must(t, err)
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != r.ID || !got[1].IncludeEnvOnDefault {
		t.Errorf("visible = %+v", got)
	}
}

// An org with no org row gets the undeclared <slug>.<instance host>, once;
// no instance row, nothing.
func TestEnsureOrg(t *testing.T) {
	_, l, orgs := world(t)
	must(t, l.EnsureOrg(ctx, "o1", "acme"))
	all, err := l.ListAll(ctx)
	must(t, err)
	if len(all) != 0 {
		t.Fatalf("no instance row, made %+v", all)
	}
	must(t, l.SeedInstance(ctx, "example.com"))
	must(t, l.EnsureOrg(ctx, "o1", "acme"))
	must(t, l.EnsureOrg(ctx, "o1", "acme"))
	all, err = l.ListAll(ctx)
	must(t, err)
	var made []store.DomainResource
	for _, r := range all {
		if r.Level == domainres.Org {
			made = append(made, r)
		}
	}
	if len(made) != 1 || made[0].Host != "acme.example.com" || made[0].Declared || *made[0].OrgID != "o1" {
		t.Errorf("org rows = %+v", made)
	}

	// globex's stack already reserved globex.example.com: finishing adds nothing.
	_, err = l.Create(ctx, domainres.Spec{Level: domainres.Stack, OwnerID: "s2", Host: "globex.example.com"}, "o2", orgs)
	must(t, err)
	must(t, l.EnsureOrg(ctx, "o2", "globex"))
	all, err = l.ListAll(ctx)
	must(t, err)
	if len(all) != 3 {
		t.Errorf("rows after a reserved default = %+v, want 3", all)
	}
}

func TestSeedInstance(t *testing.T) {
	_, l, _ := world(t)
	must(t, l.SeedInstance(ctx, ""))
	if err := l.SeedInstance(ctx, "localhost"); err == nil {
		t.Error("seeded an address")
	}
	must(t, l.SeedInstance(ctx, "*.Example.com"))
	must(t, l.SeedInstance(ctx, "other.com"))
	all, err := l.ListAll(ctx)
	must(t, err)
	if len(all) != 1 {
		t.Fatalf("rows = %d, want 1", len(all))
	}
	r := all[0]
	if r.Level != domainres.Instance || r.Host != "example.com" || r.Declared || r.OrgID != nil || r.StackID != nil {
		t.Errorf("seeded %+v", r)
	}
}
