package promote

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// syncWorld is setup with the stack managed in the UI (no config repo): dev
// is the source, prd the env that syncs.
func syncWorld(t *testing.T) *world {
	w := setup(t)
	w.st.ConfigRepo = ""
	must(t, w.s.Stacks.Update(ctx, w.st))
	return w
}

// mk creates a tile named name in e from row.
func (w *world) mk(t *testing.T, e store.Environment, name string, row store.Tile) store.Tile {
	row.StackID, row.EnvironmentID, row.Name, row.Slug = w.st.ID, e.ID, name, name
	got, err := w.f.D.Tiles.Create(ctx, row)
	must(t, err)
	return got
}

func imgTile(ref string, port int) store.Tile {
	return store.Tile{Kind: tile.Image, ImageRef: ref, ContainerPort: port}
}

func (w *world) syncPlan(t *testing.T, drop ...string) *Sync {
	s, err := w.f.SyncPlan(ctx, w.prd.ID, w.dev.ID, drop)
	must(t, err)
	return s
}

func (w *world) syncApply(t *testing.T, s *Sync, keep ...string) (*Sync, error) {
	return w.f.SyncApply(ctx, w.prd.ID, w.dev.ID, keep, s.Sig, io.Discard, nil)
}

func tags(s *Sync) string {
	var out []string
	for _, x := range s.Tiles {
		out = append(out, x.Slug+":"+x.Tag)
	}
	return strings.Join(out, " ")
}

func (w *world) running(t store.Tile) {
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID:     "run-" + t.ID,
		State:  "running",
		Labels: map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"},
	})
}

func (w *world) tileIn(t *testing.T, e store.Environment, slug string) store.Tile {
	got, err := w.f.D.Tiles.GetBySlug(ctx, e.ID, slug)
	must(t, err)
	return got
}

func TestSyncIdenticalAndVersions(t *testing.T) {
	w := syncWorld(t)
	w.mk(t, w.dev, "api", imgTile("nginx:1", 80))
	w.mk(t, w.prd, "api", imgTile("nginx:1", 80))
	if s := w.syncPlan(t); len(s.Changes) != 0 || len(s.Tiles) != 0 || s.Blocked() {
		t.Errorf("identical: %+v", s)
	}
	// Only the version differs: not a change.
	w.mk(t, w.dev, "web", imgTile("nginx:2", 80))
	w.mk(t, w.prd, "web", imgTile("nginx:3", 80))
	if s := w.syncPlan(t); len(s.Changes) != 0 || len(s.Tiles) != 0 {
		t.Errorf("tag only: %v %s", s.Changes, tags(s))
	}
}

func TestSyncNewImageTile(t *testing.T) {
	w := syncWorld(t)
	w.mk(t, w.dev, "api", imgTile("nginx:1", 80))
	s := w.syncPlan(t)
	if tags(s) != "api:new" || len(s.Changes) != 1 || s.Changes[0].Kind != "create" || s.Blocked() {
		t.Fatalf("plan: %s %v %v", tags(s), s.Changes, s.Blockers)
	}
	got, err := w.syncApply(t, s, "api")
	must(t, err)
	api := w.tileIn(t, w.prd, "api")
	if api.ImageRef != "nginx:1" || len(got.Deployed) != 1 || got.Deployed[0] != api.ID {
		t.Errorf("created %+v, deployed %v", api, got.Deployed)
	}
	if !slices.ContainsFunc(w.fake.Specs, func(sp docker.ContainerSpec) bool { return sp.Image == "nginx:1" }) {
		t.Error("nginx:1 never ran")
	}
	if s := w.syncPlan(t); len(s.Changes) != 0 {
		t.Errorf("re-plan: %v", s.Changes)
	}
}

func TestSyncNewGitTile(t *testing.T) {
	w := syncWorld(t)
	w.mk(t, w.dev, "api", store.Tile{Kind: tile.Service, GitURL: "https://github.com/acme/api", ContainerPort: 80})
	s := w.syncPlan(t)
	if len(s.Warnings) != 1 || s.Warnings[0] != "api has no build in prd yet; it starts with the next release promoted here" {
		t.Errorf("ladder warnings: %v", s.Warnings)
	}
	got, err := w.syncApply(t, s, "api")
	must(t, err)
	api := w.tileIn(t, w.prd, "api")
	if len(got.Deployed) != 0 || api.GitURL == "" {
		t.Errorf("deployed %v, tile %+v", got.Deployed, api)
	}
	// A branch env builds on its own push.
	back, err := w.f.SyncPlan(ctx, w.dev.ID, w.prd.ID, nil)
	must(t, err)
	if len(back.Changes) != 0 {
		t.Fatalf("back: %v", back.Changes)
	}
	w.mk(t, w.prd, "job", store.Tile{Kind: tile.Service, GitURL: "https://github.com/acme/job", ContainerPort: 80})
	back, err = w.f.SyncPlan(ctx, w.dev.ID, w.prd.ID, nil)
	must(t, err)
	if len(back.Warnings) != 1 || !strings.HasSuffix(back.Warnings[0], "it starts on the next push to main") {
		t.Errorf("branch warnings: %v", back.Warnings)
	}
	if len(back.Creates) != 1 || back.Creates[0].GitBranch != "main" {
		t.Errorf("creates: %+v", back.Creates)
	}
}

func TestSyncEditedKeepsTheTargetsVersion(t *testing.T) {
	w := syncWorld(t)
	dapi := w.mk(t, w.dev, "api", imgTile("nginx:2", 8080))
	papi := w.mk(t, w.prd, "api", imgTile("nginx:1", 80))
	w.mk(t, w.dev, "backup", store.Tile{Kind: tile.Cron, ImageRef: "alpine:3", Schedule: "0 1 * * *"})
	w.mk(t, w.prd, "backup", store.Tile{
		Kind: tile.Cron, ImageRef: "alpine:2", Schedule: "0 2 * * *", Paused: true,
	})
	s := w.syncPlan(t)
	if tags(s) != "api:edited backup:edited" {
		t.Fatalf("tags: %s", tags(s))
	}
	if !strings.Contains(kinds(&s.Plan), "update:apiport") || strings.Contains(kinds(&s.Plan), "image") {
		t.Errorf("changes: %s", kinds(&s.Plan))
	}
	// Not running: edited, not redeployed.
	got, err := w.syncApply(t, s, "api", "backup")
	must(t, err)
	if len(got.Deployed) != 0 {
		t.Errorf("deployed idle tile: %v", got.Deployed)
	}
	api, cron := w.tileIn(t, w.prd, "api"), w.tileIn(t, w.prd, "backup")
	if api.ContainerPort != 8080 || api.ImageRef != "nginx:1" {
		t.Errorf("api: port %d image %s", api.ContainerPort, api.ImageRef)
	}
	if cron.Schedule != "0 1 * * *" || cron.ImageRef != "alpine:2" || !cron.Paused {
		t.Errorf("cron: %+v", cron)
	}

	// Running: the next edit redeploys.
	w.running(papi)
	edit := dapi
	edit.ContainerPort = 9090
	_, _, err = w.f.D.Tiles.Update(ctx, dapi, edit)
	must(t, err)
	s = w.syncPlan(t)
	got, err = w.syncApply(t, s, "api")
	must(t, err)
	if len(got.Deployed) != 1 || got.Deployed[0] != papi.ID {
		t.Errorf("running tile not redeployed: %v", got.Deployed)
	}
}

func TestSyncParams(t *testing.T) {
	w := syncWorld(t)
	row := imgTile("nginx:1", 80)
	row.EnvJSON = `{"MODE":"${{ params.app.mode }}","KEY":"${{ params.app.key }}","TOKEN":"${{ params.app.token }}"}`
	w.mk(t, w.dev, "api", row)
	scope := params.Scope{Kind: "env", ID: w.dev.ID}
	must(t, w.f.D.Params.Merge(ctx, scope, []params.Entry{
		{Collection: "app", Name: "mode", Kind: params.Param, Value: "fast"},
		{Collection: "app", Name: "key", Kind: params.Secret, Value: "s3cret"},
		{Collection: "app", Name: "token", Kind: params.Param, Value: "dev-token"},
	}))
	// A name set in the target keeps its value.
	must(t, w.f.D.Params.Merge(ctx, params.Scope{Kind: "env", ID: w.prd.ID}, []params.Entry{
		{Collection: "app", Name: "token", Kind: params.Param, Value: "prd-token"},
	}))
	s := w.syncPlan(t)
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "params.app.key") || !strings.Contains(s.Warnings[0], "api") {
		t.Errorf("warnings: %v", s.Warnings)
	}
	for _, c := range s.Changes {
		if strings.Contains(c.Line(), "fast") || strings.Contains(c.Line(), "s3cret") {
			t.Errorf("a value leaked: %s", c.Line())
		}
	}
	// The tile reads a secret that is still unset, so its deploy parks; the
	// writes before it stand.
	parked, err := w.syncApply(t, s, "api")
	var unset errs.Unset
	if !errors.As(err, &unset) || unset.Param != "app.key" {
		t.Fatalf("err = %v", err)
	}
	api := w.tileIn(t, w.prd, "api")
	if len(parked.Owed) != 1 || parked.Owed[0] != api.ID {
		t.Errorf("owed = %v, want [%s]", parked.Owed, api.ID)
	}
	vals, err := w.f.D.Params.Values(ctx, params.Scope{Kind: "env", ID: w.prd.ID}, true)
	must(t, err)
	if vals["app.mode"].V != "fast" || vals["app.token"].V != "prd-token" {
		t.Errorf("params: %+v", vals)
	}
	if _, ok := vals["app.key"]; ok {
		t.Error("a secret was copied")
	}
	// Set, the owed deploy goes through without a plan.
	must(t, w.f.D.Params.Merge(ctx, params.Scope{Kind: "env", ID: w.prd.ID}, []params.Entry{
		{Collection: "app", Name: "key", Kind: params.Secret, Value: "prd-key"},
	}))
	done, err := w.f.SyncRollout(ctx, parked.Owed, io.Discard, nil)
	if err != nil || len(done.Deployed) != 1 || done.Deployed[0] != api.ID || len(done.Owed) != 0 {
		t.Errorf("rollout = %+v, %v", done, err)
	}
}

func TestSyncEnvNoteShowsKeysNotValues(t *testing.T) {
	w := syncWorld(t)
	d := imgTile("nginx:1", 80)
	d.EnvJSON = `{"A":"1","NEW":"topsecret"}`
	w.mk(t, w.dev, "api", d)
	p := imgTile("nginx:1", 80)
	p.EnvJSON = `{"A":"1","OLD":"x"}`
	w.mk(t, w.prd, "api", p)
	s := w.syncPlan(t)
	var note string
	for _, c := range s.Changes {
		if c.Field == "env" {
			note = c.Note
		}
	}
	if note != "+NEW −OLD" {
		t.Errorf("note = %q in %v", note, s.Changes)
	}
}

func TestSyncBlockers(t *testing.T) {
	w := syncWorld(t)
	if s := w.syncPlan(t); s.Blocked() {
		t.Fatalf("empty envs blocked: %v", s.Blockers)
	}
	same, err := w.f.SyncPlan(ctx, w.prd.ID, w.prd.ID, nil)
	must(t, err)
	if len(same.Blockers) != 1 || same.Blockers[0] != "an environment cannot sync from itself" {
		t.Errorf("same env: %v", same.Blockers)
	}
	w.st.ConfigRepo = "https://github.com/acme/shop"
	must(t, w.s.Stacks.Update(ctx, w.st))
	bound := w.syncPlan(t)
	if len(bound.Blockers) != 1 || !strings.Contains(bound.Blockers[0], "managed by its config file") {
		t.Errorf("config bound: %v", bound.Blockers)
	}
	if _, err := w.syncApply(t, bound); err == nil {
		t.Error("a blocked sync applied")
	} else if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("err = %v", err)
	}
}

func TestSyncManagedInUseBlocks(t *testing.T) {
	w := syncWorld(t)
	db := w.mk(t, w.prd, "db", store.Tile{Kind: tile.Managed})
	m, err := w.f.D.Managed.Create(ctx, db.ID, "postgres", "stackr", "")
	must(t, err)
	// A slice tile on the instance that both envs keep.
	from := "shop:prd:db"
	for _, e := range []store.Environment{w.dev, w.prd} {
		sl := w.mk(t, e, "app-db", store.Tile{Kind: tile.Slice, ProvisionFrom: &from})
		if e.ID == w.prd.ID {
			_, err = w.f.D.Managed.CreateProvision(ctx, m, sl.ID, managed.Slice{DBName: "x", DBUser: "u", DBPassword: "p"})
			must(t, err)
		}
	}
	s := w.syncPlan(t)
	if want := "tile db still holds a slice that stays; remove its slice tile first"; !slices.Contains(s.Blockers, want) {
		t.Errorf("blockers: %v, want %q", s.Blockers, want)
	}
}

// The same managed slug on another engine is an Edited tile that blocks, as
// in a promote, even when no other field differs.
func TestSyncEngineChangeBlocks(t *testing.T) {
	w := syncWorld(t)
	for env, engine := range map[store.Environment]string{w.dev: "s3", w.prd: "postgres"} {
		m := w.mk(t, env, "m3", store.Tile{Kind: tile.Managed})
		_, err := w.f.D.Managed.Create(ctx, m.ID, engine, "stackr", "")
		must(t, err)
	}
	s := w.syncPlan(t)
	want := "tile m3 changes engine (postgres to s3); a new engine is a new instance, add it under another name"
	if tags(s) != "m3:edited" || !slices.Contains(s.Blockers, want) {
		t.Errorf("tags %s, blockers %v", tags(s), s.Blockers)
	}
	if _, err := w.syncApply(t, s, "m3"); err == nil {
		t.Error("an engine change applied")
	}
}

func TestSyncUnreachableSliceBlocks(t *testing.T) {
	w := syncWorld(t)
	from := "shop:dev:nope"
	w.mk(t, w.dev, "app-db", store.Tile{Kind: tile.Slice, ProvisionFrom: &from})
	s := w.syncPlan(t)
	// "first segment" means the org list was never loaded.
	if len(s.Blockers) == 0 || !strings.HasPrefix(s.Blockers[0], "slice app-db:") ||
		strings.Contains(s.Blockers[0], "first segment") {
		t.Errorf("blockers: %v", s.Blockers)
	}
}

func TestSyncDanglingAfterDrop(t *testing.T) {
	w := syncWorld(t)
	w.mk(t, w.dev, "db", imgTile("postgres:16", 5432))
	web := imgTile("nginx:1", 80)
	web.DependsOn = "db"
	w.mk(t, w.dev, "web", web)
	if s := w.syncPlan(t); s.Blocked() || tags(s) != "db:new web:new" {
		t.Fatalf("full: %v %s", s.Blockers, tags(s))
	}
	s := w.syncPlan(t, "db")
	if len(s.Blockers) != 1 || !strings.Contains(s.Blockers[0], "tile web names db") {
		t.Errorf("blockers: %v", s.Blockers)
	}
	if !s.Tiles[0].Dropped || s.Tiles[1].Dropped {
		t.Errorf("tiles: %+v", s.Tiles)
	}
}

func TestSyncDropKeepsTheTarget(t *testing.T) {
	w := syncWorld(t)
	w.mk(t, w.prd, "old", imgTile("nginx:1", 80))
	s := w.syncPlan(t)
	if tags(s) != "old:removed" || kinds(&s.Plan) != "delete:old" {
		t.Fatalf("plan: %s %s", tags(s), kinds(&s.Plan))
	}
	s = w.syncPlan(t, "old")
	if len(s.Changes) != 0 || !s.Tiles[0].Dropped {
		t.Errorf("dropped: %v %+v", s.Changes, s.Tiles)
	}
	if _, err := w.syncApply(t, s); err == nil {
		t.Error("nothing to sync applied")
	}
	if got, _ := w.f.D.Tiles.List(ctx, w.prd.ID); len(got) != 1 {
		t.Errorf("tiles after a dropped removal: %d", len(got))
	}
}

func TestSyncSigDrift(t *testing.T) {
	w := syncWorld(t)
	d := imgTile("nginx:1", 80)
	d.EnvJSON = `{"A":"1"}`
	dev := w.mk(t, w.dev, "api", d)
	s := w.syncPlan(t)
	again := w.syncPlan(t)
	if s.Sig == "" || s.Sig != again.Sig {
		t.Fatalf("sig not stable: %q %q", s.Sig, again.Sig)
	}
	// A value edit shows nothing new in the review but moves the sig.
	edit := dev
	edit.EnvJSON = `{"A":"2"}`
	_, _, err := w.f.D.Tiles.Update(ctx, dev, edit)
	must(t, err)
	if next := w.syncPlan(t); next.Sig == s.Sig {
		t.Error("sig did not move")
	}
	_, err = w.syncApply(t, s, "api")
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("stale sig: %v", err)
	}
	if got, _ := w.f.D.Tiles.List(ctx, w.prd.ID); len(got) != 0 {
		t.Error("a stale review created a tile")
	}
}

func TestSyncRemovedManagedOrphansItsVolume(t *testing.T) {
	w := syncWorld(t)
	db := w.mk(t, w.prd, "db", store.Tile{Kind: tile.Managed})
	m, err := w.f.D.Managed.Create(ctx, db.ID, "postgres", "stackr", "")
	must(t, err)
	vol, _, err := w.f.D.Volumes.Declare(ctx, volume.Scope{Kind: "env", ID: w.prd.ID}, "db-data", 0, &m.ID)
	must(t, err)
	s := w.syncPlan(t)
	if tags(s) != "db:removed" || !strings.Contains(s.Changes[0].Note, "orphaned") {
		t.Fatalf("plan: %s %v", tags(s), s.Changes)
	}
	got, err := w.syncApply(t, s, "db")
	must(t, err)
	if len(got.Removed) != 1 || got.Removed[0] != db.ID {
		t.Errorf("removed: %v", got.Removed)
	}
	v, err := w.f.D.Volumes.Get(ctx, vol.ID)
	must(t, err)
	if v.OrphanedAt == nil {
		t.Error("the instance volume was not orphaned")
	}
}

func TestSyncVolumes(t *testing.T) {
	w := syncWorld(t)
	_, _, err := w.f.D.Volumes.Declare(ctx, volume.Scope{Kind: "env", ID: w.dev.ID}, "data", 100, nil)
	must(t, err)
	row := imgTile("nginx:1", 80)
	row.Volumes = "data:/srv"
	w.mk(t, w.dev, "api", row)
	s := w.syncPlan(t)
	if !strings.Contains(kinds(&s.Plan), "volume:") {
		t.Fatalf("plan: %s", kinds(&s.Plan))
	}
	_, err = w.syncApply(t, s, "api")
	must(t, err)
	v, err := w.f.D.Volumes.List(ctx, volume.Scope{Kind: "env", ID: w.prd.ID})
	must(t, err)
	if len(v) != 1 || v[0].Slug != "data" || v[0].MaxSizeMB != 100 {
		t.Errorf("volumes: %+v", v)
	}
}

func TestSyncReachableSlice(t *testing.T) {
	w := newSliceWorld(t)
	w.st.ConfigRepo = ""
	must(t, w.s.Stacks.Update(ctx, w.st))
	from := "infra:production:pg-db"
	w.mk(t, w.dev, "app-db", store.Tile{Kind: tile.Slice, ProvisionFrom: &from})
	if s := w.syncPlan(t); s.Blocked() || tags(s) != "app-db:new" {
		t.Errorf("plan: %v %s", s.Blockers, tags(s))
	}
}

// A slice whose source instance is stopped blocks the sync at plan time.
func TestSyncStoppedSourceBlocks(t *testing.T) {
	w := newSliceWorld(t)
	w.st.ConfigRepo = ""
	must(t, w.s.Stacks.Update(ctx, w.st))
	from := "infra:production:pg-db"
	w.mk(t, w.dev, "app-db", store.Tile{Kind: tile.Slice, ProvisionFrom: &from})
	for i, c := range w.fake.Containers {
		if c.ID == "pg-production" {
			w.fake.Containers[i].State = "exited"
		}
	}
	s := w.syncPlan(t)
	want := "slice app-db: infra/production/pg-db is not running; start it first"
	if len(s.Blockers) != 1 || s.Blockers[0] != want {
		t.Errorf("blockers: %v", s.Blockers)
	}
}

// A sync that fails before its deletes reports nothing removed, so the
// caller keeps the run history of the tiles that are still there.
func TestSyncFailedApplyRemovesNothing(t *testing.T) {
	w := syncWorld(t)
	w.mk(t, w.dev, "api", imgTile("nginx:1", 80))
	old := w.mk(t, w.prd, "old", imgTile("nginx:1", 80))
	s := w.syncPlan(t)
	// A row taking the create's slug fails the create, before any delete.
	clash := func() error { w.mk(t, w.prd, "api", imgTile("nginx:1", 80)); return nil }
	got, err := w.f.SyncApply(ctx, w.prd.ID, w.dev.ID, []string{"api", "old"}, s.Sig, io.Discard, clash)
	if err == nil || len(got.Removed) != 0 {
		t.Fatalf("apply = %v, removed %v; want an error and nothing removed", err, got.Removed)
	}
	w.tileIn(t, w.prd, old.Slug)
}
