package promote

import (
	"context"
	"io"
	"strings"
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

const xyFile = "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n" +
	"    x: {port: 80, watch_paths: [\"^x/\"]}\n    y: {port: 80, watch_paths: [\"^y/\"]}\n"

func (w *world) fakeBuild(built *[]string) {
	w.f.Build = func(ctx context.Context, _ store.Stack, tl store.Tile, commit string, _ io.Writer) (string, error) {
		*built = append(*built, tl.Slug+"@"+commit)
		im, err := w.f.D.Images.Built(ctx, "stackr/"+tl.Slug+":"+commit, "")
		return im.ID, err
	}
}

// An env that runs nothing yet: the second push is built on the first, so tile
// x is not built twice.
func TestSecondPushOnAnEnvWithNoReleaseBuildsOnTheFirst(t *testing.T) {
	w := setup(t)
	w.files["c1"], w.files["c2"] = xyFile, xyFile
	var built []string
	w.fakeBuild(&built)
	r1, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c1"}, io.Discard)
	must(t, err)
	r2, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c2", Changed: []string{"y/main.go"}}, io.Discard)
	must(t, err)
	if r2.BuiltOn == nil || *r2.BuiltOn != r1.ID {
		t.Errorf("second release built on %v, want the first", r2.BuiltOn)
	}
	if len(built) != 3 || built[2] != "y@c2" {
		t.Errorf("built %v, want x and y once, then y", built)
	}
}

// The fault itself: a waiting push release, then a cut that moves the env
// (a rebuild); the next push still builds on the waiting release's config pin.
func TestPushAfterAnEnvCutHoldsTheWaitingConfigPin(t *testing.T) {
	w := setup(t)
	w.files["c3"] = xyFile
	w.f.FileID = func(context.Context, store.Stack, string, string) (string, error) { return "same", nil }
	var built []string
	w.fakeBuild(&built)
	cfg := func(c string) release.Pin {
		return release.Pin{Slug: release.ConfigSlug, Repo: w.st.ConfigRepo, CommitSHA: c}
	}
	r5 := w.runs(t, w.dev, cfg("c1"))
	d := w.f.D.Releases
	r6, err := d.Derive(ctx, w.st.ID, r5.ID, "push", release.BranchLine("main"), cfg("c2"))
	must(t, err)
	r7, err := d.Derive(ctx, w.st.ID, r5.ID, "rebuild", release.EnvLine(w.dev.ID))
	must(t, err)
	_, err = w.f.D.Envs.SetRelease(ctx, w.dev, r7.ID)
	must(t, err)
	must(t, d.Carry(ctx, w.st.ID, r5.ID, r7.ID, release.Lines(w.dev)...))
	r, _, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c3", Changed: []string{"y/main.go"}}, io.Discard)
	must(t, err)
	pins, err := d.Pins(ctx, r.ID)
	must(t, err)
	if pins[release.ConfigSlug].CommitSHA != "c2" || r6.ID == r.ID {
		t.Errorf("config pin = %+v, want the waiting c2", pins[release.ConfigSlug])
	}
}

func autoIDs(es []store.Environment) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Slug)
	}
	return out
}

// Two quick pushes on an auto env, nothing promoted between: both come back
// auto, and the second holds the first's build.
func TestQuickPushesOnAnAutoEnvAreBothAuto(t *testing.T) {
	w := setup(t)
	w.files["c2"], w.files["c3"] = xyFile, xyFile
	var built []string
	w.fakeBuild(&built)
	ix, err := w.f.D.Images.Built(ctx, "stackr/x:c1", "")
	must(t, err)
	w.runs(t, w.dev, release.Pin{Slug: "x", Repo: w.st.ConfigRepo, CommitSHA: "c1", ImageID: &ix.ID})
	r1, auto, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c2", Changed: []string{"x/main.go"}}, io.Discard)
	must(t, err)
	if len(auto) != 1 {
		t.Fatalf("first push auto = %v", autoIDs(auto))
	}
	r2, auto, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c3", Changed: []string{"y/main.go"}}, io.Discard)
	must(t, err)
	p1, _ := w.f.D.Releases.Pins(ctx, r1.ID)
	p2, _ := w.f.D.Releases.Pins(ctx, r2.ID)
	if len(auto) != 1 || r2.BuiltOn == nil || *r2.BuiltOn != r1.ID || *p2["x"].ImageID != *p1["x"].ImageID {
		t.Errorf("second push auto = %v built on %v", autoIDs(auto), r2.BuiltOn)
	}
}

// A manual watch release waits on an auto env: the push is made but held.
func TestPushBehindAWaitingWatchIsHeld(t *testing.T) {
	w := setup(t)
	w.files["c2"] = xyFile
	var built []string
	w.fakeBuild(&built)
	cur := w.runs(t, w.dev, release.Pin{Slug: release.ConfigSlug, Repo: w.st.ConfigRepo, CommitSHA: "c1"})
	waiting, err := w.f.D.Releases.Derive(ctx, w.st.ID, cur.ID, "image-watch", release.EnvLine(w.dev.ID), release.Pin{Slug: "db", Repo: "pg:16", Digest: "sha256:d2"})
	must(t, err)
	var log strings.Builder
	r, auto, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c2"}, &log)
	must(t, err)
	want := "dev: release #3 builds on #2 which waits; not promoted automatically"
	if r.ID == "" || len(auto) != 0 || !strings.Contains(log.String(), want) || r.BuiltOn == nil || *r.BuiltOn != waiting.ID {
		t.Errorf("release %q auto %v log %q", r.ID, autoIDs(auto), log.String())
	}
}

// Rolled back to 3 with 4 and 5 waiting again: a push on an auto env builds
// on 5 and, when 4 and 5 are pushes, is promoted; when 5 is an image watch,
// it is held.
func TestPushAfterARollback(t *testing.T) {
	for _, c := range []struct {
		by   string
		line string
		want int
	}{{"push", release.BranchLine("main"), 1}, {"image-watch", release.EnvLine("dev"), 0}} {
		t.Run(c.by, func(t *testing.T) {
			w := setup(t)
			w.files["c9"] = xyFile
			var built []string
			w.fakeBuild(&built)
			cfg := func(c string) release.Pin {
				return release.Pin{Slug: release.ConfigSlug, Repo: w.st.ConfigRepo, CommitSHA: c}
			}
			r3 := w.runs(t, w.dev, cfg("c3"))
			d := w.f.D.Releases
			r4, err := d.Derive(ctx, w.st.ID, r3.ID, "push", release.BranchLine("main"), cfg("c4"))
			must(t, err)
			line := c.line
			if c.by == "image-watch" {
				line = release.EnvLine(w.dev.ID)
			}
			r5, err := d.Derive(ctx, w.st.ID, r4.ID, c.by, line, cfg("c5"))
			must(t, err)
			r, auto, err := w.f.Push(ctx, w.st.ID, Event{Repo: pushRepo, Branch: "main", Commit: "c9", Changed: []string{"y/main.go"}}, io.Discard)
			must(t, err)
			if r.BuiltOn == nil || *r.BuiltOn != r5.ID || len(auto) != c.want {
				t.Errorf("built on %v, auto %v, want %d", r.BuiltOn, autoIDs(auto), c.want)
			}
		})
	}
}
