package promote

import (
	"io"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func hasImageChange(p *Plan, slug string) bool {
	for _, c := range p.Changes {
		if c.Kind == "image" && c.Tile == slug {
			return true
		}
	}
	return false
}

// A tile started by tag and pinned by digest afterwards names the tag in its
// container; the replica's stackr.ref label says which pin it was started
// for, so a parked promote that moved the pointer still redeploys it.
func TestRunsOtherByRefLabel(t *testing.T) {
	w := syncWorld(t)
	api := w.mk(t, w.prd, "api", imgTile("nginx:1", 80))
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID:    "run-" + api.ID,
		State: "running",
		Image: "nginx:1",
		Labels: map[string]string{
			tile.LabelTile: api.ID, tile.LabelRole: "replica",
			deploy.LabelRef: "nginx@sha256:one",
		},
	})
	r2 := w.release(t, "", release.Pin{Slug: "api", Repo: "nginx", Digest: "sha256:two"})
	_, err := w.f.D.Envs.SetRelease(ctx, w.prd, r2.ID)
	must(t, err)

	p, err := w.f.Plan(ctx, w.prd.ID, r2.ID, io.Discard)
	must(t, err)
	if !hasImageChange(p, "api") {
		t.Errorf("changes = %+v; want api redeployed (runs another image)", p.Changes)
	}
}

// An on_deploy function whose pin changed after its last run is redeployed
// on resume even though the env pointer already moved: its run is queued
// from plan.Deployed, so a skipped migration is not lost.
func TestResumeRedeploysChangedOnDeployFunction(t *testing.T) {
	w := syncWorld(t)
	w.f.D.Runs = lrun.New(w.s.Runs, t.TempDir())
	row := store.Tile{Kind: tile.Function, Trigger: tile.OnDeploy, ImageRef: "alpine:1"}
	fn := w.mk(t, w.prd, "migrate", row)
	r1 := w.release(t, "", release.Pin{Slug: "migrate", Repo: "alpine", Digest: "sha256:one"})
	r2 := w.release(t, "", release.Pin{Slug: "migrate", Repo: "alpine", Digest: "sha256:two"})
	_, err := w.f.D.Envs.SetRelease(ctx, w.prd, r2.ID)
	must(t, err)
	_, err = w.f.D.Runs.Start(ctx, fn.ID, &r1.ID, lrun.Deploy)
	must(t, err)

	p, err := w.f.Plan(ctx, w.prd.ID, r2.ID, io.Discard)
	must(t, err)
	if !hasImageChange(p, "migrate") {
		t.Errorf("changes = %+v; want migrate redeployed", p.Changes)
	}
}
