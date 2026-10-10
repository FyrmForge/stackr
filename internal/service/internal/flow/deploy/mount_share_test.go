package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// Two orgs; acme has an NFS share "media" and an SMB share "docs" whose
// password is an org secret.
func shareWorld(t *testing.T) (*Flow, *dockerfake.Fake, store.Org, store.Org) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Store(t)
	fake := dockerfake.New()
	f := &Flow{
		Volumes: volume.New(st.Volumes, fake).WithShares(st.Shares),
		Params:  params.New(st.Params),
		Tiers:   tier.New(st.Tiers),
		Envs:    environment.New(st.Environments, nil),
	}
	mk := func(slug string) store.Org {
		o := store.Org{ID: "o-" + slug, Name: slug, Slug: slug, EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}
		if err := st.Orgs.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	acme, globex := mk("acme"), mk("globex")
	for _, sp := range []volume.ShareSpec{
		{Slug: "media", Kind: "nfs", Source: "nas.lan:/export", Options: "nfsvers=4"},
		{Slug: "docs", Kind: "smb", Source: "//nas.lan/docs", Options: "vers=3.0", User: "bob",
			PasswordRef: "${{ org.params.nas.pw }}"},
	} {
		if _, err := f.Volumes.CreateShare(ctx, acme.ID, sp); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Params.Set(ctx, params.Scope{Kind: "org", ID: acme.ID},
		params.Entry{Collection: "nas", Name: "pw", Kind: params.Secret, Value: "hunter2"}); err != nil {
		t.Fatal(err)
	}
	return f, fake, acme, globex
}

// bindIn is shareBind for a tile in a "dev" env of a stack of org o.
func bindIn(f *Flow, ctx context.Context, o store.Org, t store.Tile, m tile.Mount) (string, error) {
	return f.shareBind(ctx, o, store.Environment{ID: "e-" + o.ID, StackID: "s-" + o.ID, Slug: "dev", Type: "static"},
		store.Stack{ID: "s-" + o.ID, OrgID: o.ID}, t, m)
}

func mount(t *testing.T, line string) tile.Mount {
	t.Helper()
	m, err := tile.ParseMount(line)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A share line becomes a Docker volume with the right driver and opts, the
// secret expanded into it, labelled, and bound at the container path.
func TestShareBindOpts(t *testing.T) {
	f, fake, acme, _ := shareWorld(t)
	ctx := context.Background()

	bind, err := bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:media/photos/2026:/data:ro"))
	if err != nil || !strings.HasPrefix(bind, "stackr-share-") || !strings.HasSuffix(bind, ":/data:ro") {
		t.Fatalf("nfs bind = %q, %v", bind, err)
	}
	c := fake.Created[0]
	if c.Driver != "local" || c.Opts["type"] != "nfs" || c.Opts["o"] != "addr=nas.lan,nfsvers=4" ||
		c.Opts["device"] != ":/export/photos/2026" || c.Labels["stackr.share"] == "" {
		t.Errorf("nfs create = %+v", c)
	}

	if _, err = bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:docs:/d")); err != nil {
		t.Fatal(err)
	}
	c = fake.Created[1]
	if c.Opts["type"] != "cifs" || c.Opts["device"] != "//nas.lan/docs" ||
		c.Opts["o"] != "addr=nas.lan,username=bob,password=hunter2,vers=3.0" {
		t.Errorf("smb create = %+v", c)
	}

	// Another sub path is another volume.
	a, _ := bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:media/a:/x"))
	b, _ := bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:media/b:/x"))
	if a == b {
		t.Error("two sub paths got one volume")
	}
}

// An unset secret parks the deploy; it never mounts with a blank password.
func TestShareBindUnsetPassword(t *testing.T) {
	f, _, acme, _ := shareWorld(t)
	ctx := context.Background()
	if err := f.Params.Delete(ctx, params.Scope{Kind: "org", ID: acme.ID}, "nas", "pw"); err != nil {
		t.Fatal(err)
	}
	_, err := bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:docs:/d"))
	if u, ok := errs.IsUnset(err); !ok || u.Param != "org.nas.pw" {
		t.Errorf("err = %v, want parked on org.nas.pw", err)
	}
}

// A database tile, another org's share and a sub path that climbs out are
// all refused, and no Docker volume is made.
func TestShareBindRefusals(t *testing.T) {
	f, fake, acme, globex := shareWorld(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		o    store.Org
		t    store.Tile
		m    tile.Mount
		want string
	}{
		{"managed", acme, store.Tile{Slug: "pg", Kind: tile.Managed}, mount(t, "share:media/pg:/d"), "database on a share"},
		{"other org", globex, store.Tile{Slug: "web"}, mount(t, "share:media/a:/d"), "does not have"},
		{"dotdot", acme, store.Tile{Slug: "web"}, tile.Mount{Kind: tile.MountShare, Share: "media", Sub: "../x", Path: "/d"}, "plain sub path"},
	} {
		_, err := bindIn(f, ctx, c.o, c.t, c.m)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if len(fake.Created) != 0 {
		t.Errorf("a refused mount created %v", fake.Created)
	}
}

// A share is named by slug only: its id (which passes slug.Valid) would dodge
// the delete-while-mounted guard, which matches lines by slug.
func TestShareBindSlugOnly(t *testing.T) {
	f, fake, acme, _ := shareWorld(t)
	ctx := context.Background()
	ss, err := f.Volumes.Shares(ctx, acme.ID)
	if err != nil || len(ss) == 0 {
		t.Fatal(ss, err)
	}
	_, err = bindIn(f, ctx, acme, store.Tile{Slug: "web"}, tile.Mount{Kind: tile.MountShare, Share: ss[0].ID, Path: "/d"})
	if err == nil || !strings.Contains(err.Error(), "does not have") {
		t.Errorf("err = %v, want refused by id", err)
	}
	if len(fake.Created) != 0 {
		t.Errorf("created %v", fake.Created)
	}
}

// In a tiered org the login is read from the consumer env's tier block, not
// the org scope: dev has no password until its tier does.
func TestShareBindReadsTierBlock(t *testing.T) {
	f, _, acme, _ := shareWorld(t)
	ctx := context.Background()
	tr, err := f.Tiers.Create(ctx, acme.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	_, err = bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:docs:/d"))
	if u, ok := errs.IsUnset(err); !ok || u.Param != "org.nas.pw" {
		t.Fatalf("err = %v, want parked on org.nas.pw", err)
	}
	if err := f.Params.Set(ctx, params.Scope{Kind: "tier", ID: tr.ID},
		params.Entry{Collection: "nas", Name: "pw", Kind: params.Secret, Value: "tierpw"}); err != nil {
		t.Fatal(err)
	}
	if _, err = bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:docs:/d")); err != nil {
		t.Fatal(err)
	}
}

// A share is an org object: a stack-level params ref in its login (a row the
// create path would refuse, e.g. written by hand) is an error naming the
// share, never resolved from the consumer's env.
func TestShareBindRefusesStackParams(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	fake := dockerfake.New()
	f := &Flow{
		Volumes: volume.New(st.Volumes, fake).WithShares(st.Shares),
		Params:  params.New(st.Params),
		Tiers:   tier.New(st.Tiers),
		Envs:    environment.New(st.Environments, nil),
	}
	o := store.Org{ID: "o-acme", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}
	if err := st.Orgs.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := st.Shares.Create(ctx, store.Share{ID: "sh1", OrgID: o.ID, Slug: "bad", Kind: "smb", Source: "//nas.lan/x",
		User: "${{ params.nas.user }}", PasswordRef: "${{ org.params.nas.pw }}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err := bindIn(f, ctx, o, store.Tile{Slug: "web"}, mount(t, "share:bad:/d"))
	if err == nil || !strings.Contains(err.Error(), "share bad: a share login reads org.params only") {
		t.Errorf("err = %v", err)
	}
	if len(fake.Created) != 0 {
		t.Errorf("created %v", fake.Created)
	}
}

// A share login may name an unlocked tier's block with [x]; a locked one is
// refused.
func TestShareBindReadsQualifiedTier(t *testing.T) {
	f, _, acme, _ := shareWorld(t)
	ctx := context.Background()
	if _, err := f.Volumes.CreateShare(ctx, acme.ID, volume.ShareSpec{Slug: "q", Kind: "smb", Source: "//nas.lan/q",
		User: "bob", PasswordRef: "${{ org.params.nas[prod].pw }}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Tiers.Create(ctx, acme.ID, "dev"); err != nil {
		t.Fatal(err)
	}
	prod, err := f.Tiers.Create(ctx, acme.ID, "prod") // starts locked
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Params.Set(ctx, params.Scope{Kind: "tier", ID: prod.ID},
		params.Entry{Collection: "nas", Name: "pw", Kind: params.Secret, Value: "prodpw"}); err != nil {
		t.Fatal(err)
	}
	_, err = bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:q:/d"))
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("locked: err = %v, want a lock refusal", err)
	}
	if _, err := f.Tiers.SetLocked(ctx, prod, false); err != nil {
		t.Fatal(err)
	}
	if _, err = bindIn(f, ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:q:/d")); err != nil {
		t.Fatalf("unlocked: %v", err)
	}
}

// A lock change or a rename moves the template folder's version with no
// param row touched.
func TestParamsVersionFollowsLocks(t *testing.T) {
	f, _, acme, _ := shareWorld(t)
	ctx := context.Background()
	e := store.Environment{ID: "e-acme", StackID: "s-acme", Slug: "dev", Type: "static"}
	st := store.Stack{ID: "s-acme", OrgID: acme.ID}
	ver := func() string {
		v, err := f.paramsVersion(ctx, e, st)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v0 := ver()
	dev, err := f.Tiers.Create(ctx, acme.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	v1 := ver()
	if _, err := f.Tiers.SetLocked(ctx, dev, false); err != nil {
		t.Fatal(err)
	}
	v2 := ver()
	if _, err := f.Tiers.Rename(ctx, dev, "build"); err != nil {
		t.Fatal(err)
	}
	v3 := ver()
	for i, p := range [][2]string{{v0, v1}, {v1, v2}, {v2, v3}} {
		if p[0] == p[1] {
			t.Errorf("step %d: version %s did not change", i, p[0])
		}
	}
}
