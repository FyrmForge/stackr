package service

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// builtImage is a stackr build on the box, old enough for cleanup to take.
func (w *world) builtImage(t *testing.T, ref string) string {
	t.Helper()
	ctx := context.Background()
	im, err := w.orch.images.Built(ctx, ref, "sha256:"+ref)
	must(t, err)
	aged := time.Now().Add(-2 * image.Grace)
	im.BuiltAt = &aged
	must(t, w.st.Images.Update(ctx, im))
	w.fake.Images = append(w.fake.Images, docker.Image{
		ID: "sha256:" + ref, Tags: []string{ref}, Size: 100,
		Labels: map[string]string{image.LabelBuilt: "true"},
	})
	return im.ID
}

func (w *world) release(t *testing.T, imageID string) string {
	t.Helper()
	r, err := w.orch.releases.Create(context.Background(), w.stack, "test",
		[]release.Pin{{Slug: "web", Repo: "r", ImageID: &imageID}})
	must(t, err)
	return r.ID
}

func (w *world) cleanup(t *testing.T) string {
	t.Helper()
	var log bytes.Buffer
	must(t, w.orch.runCleanup(context.Background(), &jobs.Run{Log: &log}))
	return log.String()
}

func (w *world) refs() []string {
	var out []string
	for _, im := range w.fake.Images {
		out = append(out, im.Tags...)
	}
	return out
}

// job writes a promote job row: the history cleanup reads rollback depth from.
func (w *world) job(t *testing.T, env, rel, state string, age time.Duration) {
	t.Helper()
	must(t, w.st.Jobs.Create(context.Background(), store.Job{
		ID:        uuid.NewString(),
		Kind:      string(kindPromote),
		State:     state,
		ReleaseID: &rel,
		Payload:   `{"env_id":"` + env + `","release_id":"` + rel + `"}`,
		CreatedAt: time.Now().Add(-age),
	}))
}

// The sweep keeps what runs, an env's current release and the newest release
// of a stack; it drops the rest, never an image stackr did not build.
func TestCleanupKeepSet(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.orch.SetSetting(ctx, "cleanup_enabled", "true"))

	w.builtImage(t, "stkr/run:1")
	w.fake.Containers = append(w.fake.Containers, docker.Container{ID: "c1", Image: "stkr/run:1", State: "running"})
	current := w.builtImage(t, "stkr/cur:1") // the env's release, far back in history
	curRel := w.release(t, current)
	e, err := w.st.Environments.Get(ctx, w.env)
	must(t, err)
	e.ReleaseID = &curRel
	must(t, w.st.Environments.Update(ctx, e))
	w.builtImage(t, "stkr/gone:1") // unreferenced
	w.release(t, w.builtImage(t, "stkr/old:1"))
	w.release(t, w.builtImage(t, "stkr/newest:1"))
	w.fake.Images = append(w.fake.Images, docker.Image{ID: "sha256:foreign", Tags: []string{"redis:7"}, Size: 50})

	log := w.cleanup(t)
	have := strings.Join(w.refs(), " ")
	for _, keep := range []string{"stkr/run:1", "stkr/cur:1", "stkr/newest:1", "redis:7"} {
		if !strings.Contains(have, keep) {
			t.Errorf("%s was removed; left: %s", keep, have)
		}
	}
	for _, gone := range []string{"stkr/gone:1", "stkr/old:1"} {
		if strings.Contains(have, gone) {
			t.Errorf("%s kept: %s", gone, have)
		}
	}
	for _, want := range []string{"dangling images: ", "built images: 2 removed, up to 200 B", "build cache: "} {
		if !strings.Contains(log, want) {
			t.Errorf("log misses %q:\n%s", want, log)
		}
	}
}

// A promote parked on host access, or queued behind a lock, targets a release
// that may be far back; its image must survive or the release never deploys.
func TestCleanupKeepsLiveJobRelease(t *testing.T) {
	w := newWorld(t)
	must(t, w.orch.SetSetting(context.Background(), "cleanup_enabled", "true"))
	parked := w.release(t, w.builtImage(t, "stkr/parked:1"))
	for i := range rollbackDepth + 1 {
		w.release(t, w.builtImage(t, "stkr/n:"+string(rune('a'+i))))
	}
	w.job(t, w.env, parked, "waiting", time.Hour)
	w.cleanup(t)
	if have := strings.Join(w.refs(), " "); !strings.Contains(have, "stkr/parked:1") {
		t.Errorf("a parked promote's image was removed; left: %s", have)
	}
}

// Rollback depth is the last releases each env was on, not the last of the
// stack, and a release past it loses its Docker image but keeps its pin.
func TestCleanupDepthIsPerEnv(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.orch.SetSetting(ctx, "cleanup_enabled", "true"))
	prod := uuid.NewString()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: prod, StackID: w.stack, Name: "prod", Slug: "prod", Type: "static", Settings: "{}",
		Network: "p", FromKind: "branch", FromBranch: "main", CreatedAt: time.Now(),
	}))
	var rels []string
	var imgs []string
	for i := range 2 * rollbackDepth {
		ref := "stkr/r:" + string(rune('a'+i))
		imgs = append(imgs, w.builtImage(t, ref))
		rels = append(rels, w.release(t, imgs[i]))
	}
	// dev walked through every release; prod only ever went a, b.
	for i, r := range rels {
		w.job(t, w.env, r, "done", time.Duration(len(rels)-i)*time.Minute)
	}
	w.job(t, prod, rels[0], "done", 2*time.Hour)
	w.job(t, prod, rels[1], "done", time.Hour)
	for env, rel := range map[string]string{w.env: rels[len(rels)-1], prod: rels[1]} {
		e, err := w.st.Environments.Get(ctx, env)
		must(t, err)
		e.ReleaseID = &rel
		must(t, w.st.Environments.Update(ctx, e))
	}
	w.cleanup(t)
	have := strings.Join(w.refs(), " ")
	for _, keep := range []string{"stkr/r:a", "stkr/r:b", "stkr/r:j", "stkr/r:f"} {
		if !strings.Contains(have, keep) {
			t.Errorf("%s was removed; left: %s", keep, have)
		}
	}
	if strings.Contains(have, "stkr/r:c") {
		t.Errorf("a release beyond every env's depth kept: %s", have)
	}
	pins, err := w.orch.releases.Pins(ctx, rels[2])
	must(t, err)
	if pins["web"].ImageID == nil {
		t.Error("a cleaned release lost its image pin")
	}
}

func TestCleanupOffDoesNothing(t *testing.T) {
	w := newWorld(t)
	w.builtImage(t, "stkr/gone:1")
	before := len(w.fake.Calls())
	if log := w.cleanup(t); log != "" {
		t.Errorf("log: %q", log)
	}
	for _, c := range w.fake.Calls()[before:] {
		if strings.HasPrefix(c.Method, "Prune") || c.Method == "BuildCachePrune" || c.Method == "RemoveImage" {
			t.Errorf("cleanup off called %s", c)
		}
	}
	if len(w.refs()) != 1 {
		t.Errorf("images changed: %v", w.refs())
	}
}
