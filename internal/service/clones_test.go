package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/google/uuid"
)

func plant(t *testing.T, path string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o750))
	must(t, os.WriteFile(path, nil, 0o600))
}

func gone(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

// seedRow writes a seed job row in state reading the dump at path.
func seedRow(t *testing.T, w *world, state, path string) {
	t.Helper()
	p, err := json.Marshal(seedJob{Source: seedSource{Kind: "upload", Path: path}})
	must(t, err)
	j := store.Job{
		ID: uuid.NewString(), Kind: string(kindSeed), State: state,
		LockSet: store.StringList{uuid.NewString()}, Payload: string(p), CreatedAt: time.Now(),
	}
	if state == "waiting" { // an approval no grant answers: it stays parked
		ask := hostgrant.Text([]string{"/x"})
		j.WaitingParam = &ask
	}
	must(t, w.st.Jobs.Create(context.Background(), j))
}

// A boot keeps the dumps live seed jobs read and the clones a tile, stack,
// org or the server config maps to; everything else in seeds/ and repos/
// goes, and a seed job the restart fails takes its dump along.
func TestBootSweepsSeedsAndRepos(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.orch.gitEnv = []string{} // no connector in this world
	dir := w.orch.cfg.DataDir
	tl, err := w.orch.CreateTile(ctx, Tile{
		StackID: w.stack, EnvironmentID: w.env, Name: "api",
		Kind: "service", GitURL: "https://github.com/acme/api",
	})
	must(t, err)
	st, err := w.orch.stacks.Get(ctx, w.stack)
	must(t, err)
	st.ConfigRepo = "acme/shop"
	must(t, w.st.Stacks.Update(ctx, st))
	must(t, w.orch.Close())

	seeds, repos := filepath.Join(dir, "seeds"), filepath.Join(dir, "repos")
	waiting, running, stray := filepath.Join(seeds, "w.dump"), filepath.Join(seeds, "r.dump"), filepath.Join(seeds, "s.dump")
	for _, p := range []string{waiting, running, stray} {
		plant(t, p)
	}
	seedRow(t, w, "waiting", waiting)
	seedRow(t, w, "running", running)
	tileDir := filepath.Join(repos, tileCheckout(st, tl))
	keep := []string{tileDir, filepath.Join(repos, "config-"+w.stack)}
	drop := []string{
		filepath.Join(repos, "tile-"+w.stack+"-old-slug-0000"),
		filepath.Join(repos, "config-"+uuid.NewString()),
		filepath.Join(repos, "orgconfig-"+w.org), // the org binds no config repo
		filepath.Join(repos, "serverconfig"),     // nor does the server
	}
	for _, d := range append(keep, drop...) {
		plant(t, filepath.Join(d, ".git", "HEAD"))
	}
	// a link out of repos/ goes itself; what it points at stays
	outside := filepath.Join(t.TempDir(), "keep")
	plant(t, outside)
	link := filepath.Join(repos, "linked")
	must(t, os.Symlink(filepath.Dir(outside), link))

	o2, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = o2.Close() })

	for _, p := range append([]string{waiting}, keep...) {
		if gone(t, p) {
			t.Errorf("%s removed, want kept", p)
		}
	}
	for _, p := range append([]string{running, stray, link}, drop...) {
		if !gone(t, p) {
			t.Errorf("%s kept, want removed", p)
		}
	}
	if gone(t, outside) {
		t.Error("the sweep followed a symlink out of repos/")
	}
}

// A seed job cancelled before it ran drops its dump.
func TestSeedEndedDropsDump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.dump")
	plant(t, path)
	p, err := json.Marshal(seedJob{Source: seedSource{Kind: "upload", Path: path}})
	must(t, err)
	seedEnded(store.Job{Kind: "deploy", Payload: string(p)})
	if gone(t, path) {
		t.Fatal("another kind's end removed the dump")
	}
	seedEnded(store.Job{Kind: string(kindSeed), State: "cancelled", Payload: string(p)})
	if !gone(t, path) {
		t.Fatal("the cancelled seed's dump is still there")
	}
}

// Deleting a tile removes its build clone once no tile in another env
// builds from it; deleting a stack or an org removes its config clone.
func TestDeleteRemovesClones(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.orch.gitEnv = []string{} // no connector in this world
	repos := filepath.Join(w.orch.cfg.DataDir, "repos")
	edge := w.newEnv(t, "edge", "static")
	var ids []string
	for _, env := range []string{w.env, edge} {
		tl, err := w.orch.CreateTile(ctx, Tile{
			StackID: w.stack, EnvironmentID: env, Name: "api",
			Kind: "service", GitURL: "https://github.com/acme/api",
		})
		must(t, err)
		ids = append(ids, tl.ID)
	}
	st, err := w.orch.stacks.Get(ctx, w.stack)
	must(t, err)
	tl, err := w.orch.tiles.Get(ctx, ids[0])
	must(t, err)
	clone := filepath.Join(repos, tileCheckout(st, tl))
	plant(t, filepath.Join(clone, ".git", "HEAD"))

	del := func(id string) {
		t.Helper()
		j, err := w.orch.DeleteTile(ctx, id)
		must(t, err)
		if j = w.wait(t, j.ID); j.State != "done" {
			t.Fatalf("delete: %s %s", j.State, j.Error)
		}
	}
	del(ids[0])
	if gone(t, clone) {
		t.Fatal("clone removed while the edge tile still builds from it")
	}
	del(ids[1])
	if !gone(t, clone) {
		t.Fatal("clone kept after its last tile went")
	}

	other, err := w.orch.CreateStack(ctx, w.org, "other", "")
	must(t, err)
	stackDirs := []string{filepath.Join(repos, "config-"+other.ID), filepath.Join(repos, "tile-"+other.ID+"-api-0000")}
	mine := filepath.Join(repos, "config-"+w.stack)
	for _, d := range append(stackDirs, mine) {
		plant(t, filepath.Join(d, ".git", "HEAD"))
	}
	must(t, w.orch.DeleteStack(ctx, other.ID))
	for _, d := range stackDirs {
		if !gone(t, d) {
			t.Errorf("%s kept after its stack went", d)
		}
	}
	if gone(t, mine) {
		t.Error("another stack's config clone went too")
	}

	org := uuid.NewString()
	must(t, w.st.Orgs.Create(ctx, store.Org{
		ID: org, Name: "beta", Slug: "beta", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
	}))
	orgDir := filepath.Join(repos, "orgconfig-"+org)
	plant(t, filepath.Join(orgDir, ".git", "HEAD"))
	must(t, w.orch.DeleteOrg(ctx, org))
	if !gone(t, orgDir) {
		t.Error("org config clone kept after its org went")
	}
}
