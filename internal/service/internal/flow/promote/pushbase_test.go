package promote

import (
	"context"
	"io"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const pushRepo = "git@github.com:acme/shop.git"

func (w *world) runs(t *testing.T, env store.Environment, pins ...release.Pin) store.Release {
	t.Helper()
	r, err := w.f.D.Releases.Create(ctx, w.st.ID, "test", pins)
	must(t, err)
	_, err = w.f.D.Envs.SetRelease(ctx, env, r.ID)
	must(t, err)
	return r
}

// A watch release waits on dev, then a config push comes in: the push builds
// on the waiting release and keeps its digest instead of the env's old one.
func TestPushBuildsOnAWaitingWatchRelease(t *testing.T) {
	w := setup(t)
	w.files["c2"] = "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n    api: {image: nginx:1, port: 80}\n"
	cur := w.runs(t, w.dev, release.Pin{Slug: release.ConfigSlug, Repo: w.st.ConfigRepo, CommitSHA: "c1"}, release.Pin{Slug: "api", Repo: "nginx:1", Digest: "sha256:d1"})
	waiting, err := w.f.D.Releases.Derive(ctx, w.st.ID, cur.ID, "image-watch", release.EnvLine(w.dev.ID), release.Pin{Slug: "api", Repo: "nginx:1", Digest: "sha256:d2"})
	must(t, err)
	r, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c2"}, io.Discard)
	must(t, err)
	pins, err := w.f.D.Releases.Pins(ctx, r.ID)
	must(t, err)
	if pins["api"].Digest != "sha256:d2" || pins[release.ConfigSlug].CommitSHA != "c2" {
		t.Errorf("pins = %+v, want the waiting digest and the new config commit", pins)
	}
	if r.BuiltOn == nil || *r.BuiltOn != waiting.ID || r.Line == nil || *r.Line != release.BranchLine("main") {
		t.Errorf("base and line = %v %v", r.BuiltOn, r.Line)
	}
}

// Two pushes wait on dev: the first rebuilt x, the second touches only y. The
// second release carries the new x image, and x is not built a second time.
func TestSecondWaitingPushCarriesTheFirstBuild(t *testing.T) {
	w := setup(t)
	file := func() string {
		return "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n" +
			"    x: {port: 80, watch_paths: [\"^x/\"]}\n    y: {port: 80, watch_paths: [\"^y/\"]}\n"
	}
	w.files["c1"], w.files["c2"], w.files["c3"] = file(), file(), file()
	var built []string
	w.f.Build = func(ctx context.Context, _ store.Stack, tl store.Tile, commit string, _ io.Writer) (string, error) {
		built = append(built, tl.Slug+"@"+commit)
		im, err := w.f.D.Images.Built(ctx, "stackr/"+tl.Slug+":"+commit, "")
		return im.ID, err
	}
	ix, err := w.f.D.Images.Built(ctx, "stackr/x:c1", "")
	must(t, err)
	iy, err := w.f.D.Images.Built(ctx, "stackr/y:c1", "")
	must(t, err)
	x0, y0 := ix.ID, iy.ID
	w.runs(t, w.dev,
		release.Pin{Slug: release.ConfigSlug, Repo: w.st.ConfigRepo, CommitSHA: "c1"},
		release.Pin{Slug: "x", Repo: w.st.ConfigRepo, CommitSHA: "c1", ImageID: &x0},
		release.Pin{Slug: "y", Repo: w.st.ConfigRepo, CommitSHA: "c1", ImageID: &y0})
	r1, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c2", Changed: []string{"x/main.go"}}, io.Discard)
	must(t, err)
	r2, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c3", Changed: []string{"y/main.go"}}, io.Discard)
	must(t, err)
	if len(built) != 2 || built[0] != "x@c2" || built[1] != "y@c3" {
		t.Errorf("built %v, want x once and y once", built)
	}
	p1, _ := w.f.D.Releases.Pins(ctx, r1.ID)
	p2, _ := w.f.D.Releases.Pins(ctx, r2.ID)
	if p2["x"].ImageID == nil || *p2["x"].ImageID != *p1["x"].ImageID || *p2["x"].ImageID == x0 {
		t.Errorf("second release x = %v, want the first push's build", p2["x"].ImageID)
	}
	if *p2["y"].ImageID == y0 || p2[release.ConfigSlug].CommitSHA != "c3" {
		t.Errorf("second release y/config = %+v", p2)
	}
}
