package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
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

	bind, err := f.shareBind(ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:media/photos/2026:/data:ro"))
	if err != nil || !strings.HasPrefix(bind, "stackr-share-") || !strings.HasSuffix(bind, ":/data:ro") {
		t.Fatalf("nfs bind = %q, %v", bind, err)
	}
	c := fake.Created[0]
	if c.Driver != "local" || c.Opts["type"] != "nfs" || c.Opts["o"] != "addr=nas.lan,nfsvers=4" ||
		c.Opts["device"] != ":/export/photos/2026" || c.Labels["stackr.share"] == "" {
		t.Errorf("nfs create = %+v", c)
	}

	if _, err = f.shareBind(ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:docs:/d")); err != nil {
		t.Fatal(err)
	}
	c = fake.Created[1]
	if c.Opts["type"] != "cifs" || c.Opts["device"] != "//nas.lan/docs" ||
		c.Opts["o"] != "addr=nas.lan,username=bob,password=hunter2,vers=3.0" {
		t.Errorf("smb create = %+v", c)
	}

	// Another sub path is another volume.
	a, _ := f.shareBind(ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:media/a:/x"))
	b, _ := f.shareBind(ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:media/b:/x"))
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
	_, err := f.shareBind(ctx, acme, store.Tile{Slug: "web"}, mount(t, "share:docs:/d"))
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
		_, err := f.shareBind(ctx, c.o, c.t, c.m)
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
	_, err = f.shareBind(ctx, acme, store.Tile{Slug: "web"}, tile.Mount{Kind: tile.MountShare, Share: ss[0].ID, Path: "/d"})
	if err == nil || !strings.Contains(err.Error(), "does not have") {
		t.Errorf("err = %v, want refused by id", err)
	}
	if len(fake.Created) != 0 {
		t.Errorf("created %v", fake.Created)
	}
}
