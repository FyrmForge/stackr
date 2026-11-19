package release_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReleases(t *testing.T) {
	st := servicetest.Store(t)
	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        org,
		Name:      "o",
		Slug:      "o",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        stack,
		OrgID:     org,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		CreatedAt: now,
	}))
	img1, img2 := "i1", "i2"
	for _, id := range []string{img1, img2} {
		must(t, st.Images.Create(ctx, store.Image{
			ID:        id,
			Ref:       "stkr/api:" + id,
			Digest:    "sha256:" + id,
			CreatedAt: now,
		}))
	}
	l := release.New(st.Releases, st.ReleaseTiles)

	if _, err := l.Create(ctx, stack, "push", []release.Pin{{Slug: "db", Digest: "16"}}); err == nil {
		t.Error("a tag accepted as a digest")
	}
	if _, err := l.Create(ctx, stack, "push", []release.Pin{{Slug: "a"}, {Slug: "a"}}); err == nil {
		t.Error("duplicate slug accepted")
	}
	r1, err := l.Create(ctx, stack, "push", []release.Pin{
		{
			Slug:      "config",
			Repo:      "acme/stack",
			Branch:    "main",
			CommitSHA: "c1",
		},
		{
			Slug:      "api",
			Repo:      "acme/api",
			Branch:    "main",
			CommitSHA: "a1",
			ImageID:   &img1,
		},
		{
			Slug:   "db",
			Digest: "sha256:pg16",
		},
	})
	must(t, err)
	// A push to the api repo rebuilds api only; everything else is copied.
	r2, err := l.Derive(ctx, stack, r1.ID, "push", "", release.Pin{
		Slug:      "api",
		Repo:      "acme/api",
		Branch:    "main",
		CommitSHA: "a2",
		ImageID:   &img2,
	})
	must(t, err)
	if r1.Number != 1 || r2.Number != 2 {
		t.Errorf("numbers = %d, %d", r1.Number, r2.Number)
	}
	p1, _ := l.Pins(ctx, r1.ID)
	p2, _ := l.Pins(ctx, r2.ID)
	if got := release.Diff(p1, p2); !reflect.DeepEqual(got, []release.Change{{Slug: "api", Kind: "changed"}}) {
		t.Errorf("diff = %v", got)
	}
	delete(p2, "db")
	p2["web"] = release.Pin{Slug: "web", Digest: "sha256:w"}
	if got := release.Diff(p1, p2); !reflect.DeepEqual(got, []release.Change{
		{"api", "changed"},
		{"db", "removed"},
		{"web", "added"},
	}) {
		t.Errorf("diff = %v", got)
	}
	if list, _ := l.List(ctx, stack); len(list) != 2 || list[0].ID != r2.ID {
		t.Errorf("list = %v", list)
	}
	ids, _ := l.ImageIDs(ctx)
	if len(ids) != 2 {
		t.Errorf("image ids = %v", ids)
	}
	got, _ := l.GetByNumber(ctx, stack, 2)
	if got.ID != r2.ID || got.CreatedBy != "push" {
		t.Errorf("by number = %+v", got)
	}
}

func TestListCommits(t *testing.T) {
	st := servicetest.Store(t)
	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "o", Slug: "o", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "s", Slug: "s", Settings: "{}", CreatedAt: now}))
	l := release.New(st.Releases, st.ReleaseTiles)
	r1, err := l.Create(ctx, stack, "push", []release.Pin{
		{Slug: release.ConfigSlug, CommitSHA: "c1"},
		{Slug: "api", CommitSHA: "a1"},
	})
	must(t, err)
	r2, err := l.Create(ctx, stack, "push", []release.Pin{{Slug: "api", CommitSHA: "a2"}})
	must(t, err)
	list, err := l.List(ctx, stack)
	must(t, err)
	if list[0].ID != r2.ID || list[0].Commit != "" || list[1].Commit != "c1" {
		t.Errorf("list commits = %q, %q (a tile sha must not stand in for the config's)", list[0].Commit, list[1].Commit)
	}
	if g, _ := l.Get(ctx, r1.ID); g.Commit != "c1" {
		t.Errorf("get commit = %q", g.Commit)
	}
	if g, _ := l.GetByNumber(ctx, stack, 2); g.Commit != "" {
		t.Errorf("by number commit = %q", g.Commit)
	}
}

func TestSetMessage(t *testing.T) {
	st := servicetest.Store(t)
	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "o", Slug: "o", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "s", Slug: "s", Settings: "{}", CreatedAt: now}))
	l := release.New(st.Releases, st.ReleaseTiles)
	r1, err := l.Create(ctx, stack, "push", nil)
	must(t, err)
	r2, err := l.Create(ctx, stack, "push", nil)
	must(t, err)
	_, err = l.SetMessage(ctx, r1, " fix the thing\n\nlong body")
	must(t, err)
	_, err = l.SetMessage(ctx, r2, "")
	must(t, err)
	if g, _ := l.Get(ctx, r1.ID); g.Message != "fix the thing" {
		t.Errorf("message = %q", g.Message)
	}
	if g, _ := l.Get(ctx, r2.ID); g.Message != "" {
		t.Errorf("empty message stored as %q", g.Message)
	}
	ctl, err := l.SetMessage(ctx, r2, "a\x1b[31mb\tc\r")
	must(t, err)
	if ctl.Message != "a[31mbc" {
		t.Errorf("control characters kept: %q", ctl.Message)
	}
	long, err := l.SetMessage(ctx, r2, strings.Repeat("é", 300))
	must(t, err)
	if n := len([]rune(long.Message)); n != 200 {
		t.Errorf("capped to %d runes", n)
	}
}

// Newest picks the highest-numbered release of the asked lines that is built,
// through others, on what the env runs; anything else, a release cut before
// the migration, or a deleted link in the chain falls back to what it runs.
func TestNewest(t *testing.T) {
	st := servicetest.Store(t)
	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "o", Slug: "o", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "s", Slug: "s", Settings: "{}", CreatedAt: now}))
	l := release.New(st.Releases, st.ReleaseTiles)
	pin := release.Pin{Slug: "api", Digest: "sha256:a"}
	runs, err := l.Create(ctx, stack, "apply", []release.Pin{pin}) // before the migration: no base, no line
	must(t, err)
	newest := func(runs string, lines ...string) string {
		t.Helper()
		id, err := l.Newest(ctx, stack, runs, lines...)
		must(t, err)
		return id
	}
	main, dev, feat := release.BranchLine("main"), release.EnvLine("dev"), release.BranchLine("feat")
	if got := newest(runs.ID, main, dev); got != runs.ID {
		t.Errorf("no waiting release: %s, want what runs", got)
	}
	r2, err := l.Derive(ctx, stack, runs.ID, "push", main, pin)
	must(t, err)
	r3, err := l.Derive(ctx, stack, r2.ID, "image-watch", dev, pin) // built on a waiting one
	must(t, err)
	other, err := l.Derive(ctx, stack, runs.ID, "push", feat, pin) // another branch's, higher number
	must(t, err)
	if got := newest(runs.ID, main, dev); got != r3.ID {
		t.Errorf("chain through a waiting release: %s, want %s", got, r3.ID)
	}
	if got := newest(runs.ID, feat); got != other.ID {
		t.Errorf("the other branch's line: %s, want %s", got, other.ID)
	}
	// An env that runs r2 takes r3 (built on it), never the feat release.
	if got := newest(r2.ID, main, dev); got != r3.ID {
		t.Errorf("env on r2: %s, want %s", got, r3.ID)
	}
	// An env on another release is not on the line: what it runs stays.
	if got := newest(other.ID, main, dev); got != other.ID {
		t.Errorf("env off the line: %s, want what it runs", got)
	}
	// Nothing run yet: a line's own root counts, a pre-migration release does not.
	root, err := l.Derive(ctx, stack, "", "push", main, pin)
	must(t, err)
	if got := newest("", main); got != root.ID {
		t.Errorf("env running nothing: %s, want the line's root %s", got, root.ID)
	}
	// A deleted base breaks the chain: not on the line.
	must(t, st.Releases.Delete(ctx, r2.ID))
	if got := newest(runs.ID, main, dev); got != runs.ID {
		t.Errorf("deleted link: %s, want what runs", got)
	}
}

func TestCarry(t *testing.T) {
	st := servicetest.Store(t)
	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "o", Slug: "o", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "s", Slug: "s", Settings: "{}", CreatedAt: now}))
	l := release.New(st.Releases, st.ReleaseTiles)
	main := release.BranchLine("main")
	cfg := func(sha string) release.Pin { return release.Pin{Slug: release.ConfigSlug, CommitSHA: sha} }
	api := func(sha, img string) release.Pin { return release.Pin{Slug: "api", CommitSHA: sha, ImageID: &img} }
	img := func(slug, d string) release.Pin { return release.Pin{Slug: slug, Digest: "sha256:" + d} }
	for _, id := range []string{"i1", "i2", "i3"} {
		must(t, st.Images.Create(ctx, store.Image{ID: id, Ref: "r/" + id, Digest: "sha256:" + id, CreatedAt: now}))
	}
	was, err := l.Create(ctx, stack, "apply", []release.Pin{cfg("c1"), api("a1", "i1"), img("db", "d1")})
	must(t, err)
	// A cut on was, as a rebuild makes it.
	cut := func(by string, swap ...release.Pin) store.Release {
		r, err := l.Derive(ctx, stack, was.ID, by, release.EnvLine("e"), swap...)
		must(t, err)
		return r
	}
	count := func() int { rs, err := l.List(ctx, stack); must(t, err); return len(rs) }
	now1 := cut("rebuild", api("a1", "i2"))
	n := count()
	must(t, l.Carry(ctx, stack, was.ID, now1.ID, main, release.EnvLine("e")))
	if count() != n {
		t.Fatal("Carry cut a release with none waiting")
	}

	w, err := l.Derive(ctx, stack, was.ID, "push", main, cfg("c2"), api("a2", "i3"))
	must(t, err)
	w, err = l.SetMessage(ctx, w, "waiting message")
	must(t, err)
	n = count()
	must(t, l.Carry(ctx, stack, was.ID, now1.ID, main, release.EnvLine("e")))
	if count() != n+1 {
		t.Fatalf("releases %d, want %d", count(), n+1)
	}
	rs, err := l.List(ctx, stack)
	must(t, err)
	c := rs[0]
	if c.Number != w.Number+1 || c.BuiltOn == nil || *c.BuiltOn != now1.ID || c.Line == nil || *c.Line != main ||
		c.CreatedBy != "push" || c.Message != "waiting message" {
		t.Errorf("carried release = %+v", c)
	}
	pins, err := l.Pins(ctx, c.ID)
	must(t, err)
	// The waiting release changed api to a newer commit: its pin stays. Its
	// config pin is carried, and the untouched db pin too.
	if pins["api"].CommitSHA != "a2" || *pins["api"].ImageID != "i3" || pins[release.ConfigSlug].CommitSHA != "c2" || pins["db"].Digest != "sha256:d1" {
		t.Errorf("pins = %+v", pins)
	}
	if got, err := l.Newest(ctx, stack, now1.ID, main, release.EnvLine("e")); err != nil || got != c.ID {
		t.Errorf("Newest on the new run = %s %v, want the carried release", got, err)
	}

	// Same commit, fresh image wins.
	_, err = l.Derive(ctx, stack, was.ID, "push", main, cfg("c2"))
	must(t, err)
	now2 := cut("rebuild", api("a1", "i2"))
	must(t, l.Carry(ctx, stack, was.ID, now2.ID, main, release.EnvLine("e")))
	rs, _ = l.List(ctx, stack)
	pins, _ = l.Pins(ctx, rs[0].ID)
	if *pins["api"].ImageID != "i2" || pins[release.ConfigSlug].CommitSHA != "c2" {
		t.Errorf("same commit: pins = %+v", pins)
	}

	// An image digest clash: the fresh digest wins.
	w3, err := l.Derive(ctx, stack, was.ID, "image-watch", release.EnvLine("e"), img("db", "d2"))
	must(t, err)
	now3 := cut("deploy", img("db", "d3"))
	must(t, l.Carry(ctx, stack, was.ID, now3.ID, release.EnvLine("e")))
	rs, _ = l.List(ctx, stack)
	pins, _ = l.Pins(ctx, rs[0].ID)
	if rs[0].BuiltOn == nil || *rs[0].BuiltOn != now3.ID || pins["db"].Digest != "sha256:d3" || w3.ID == rs[0].ID {
		t.Errorf("digest clash: %+v %+v", rs[0], pins)
	}

	// The env ran nothing before.
	root, err := l.Derive(ctx, stack, "", "push", main, cfg("c9"))
	must(t, err)
	first, err := l.Derive(ctx, stack, "", "deploy", release.EnvLine("n"), img("db", "d9"))
	must(t, err)
	must(t, l.Carry(ctx, stack, "", first.ID, main, release.EnvLine("n")))
	rs, _ = l.List(ctx, stack)
	pins, _ = l.Pins(ctx, rs[0].ID)
	if rs[0].BuiltOn == nil || *rs[0].BuiltOn != first.ID || pins[release.ConfigSlug].CommitSHA != "c9" || pins["db"].Digest != "sha256:d9" || root.ID == rs[0].ID {
		t.Errorf("was empty: %+v %+v", rs[0], pins)
	}
}
