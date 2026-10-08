package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// A panel restart gives a fresh vip.Table; without the boot rebuild the first
// route redeclares STACKR-VIP and drops every other tile's jump.
func TestRebuildVIPsKeepsOtherTiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fake := dockerfake.New()
	var scripts []string
	table := vip.New() // the restarted panel's empty table
	table.Restore = func(_ context.Context, s string) error { scripts = append(scripts, s); return nil }
	table.Exec = func(context.Context, ...string) error { return nil }
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(fake),
		WithVIP(table),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	st := storetest.Open(t, filepath.Join(dir, "stackr.db"))
	org, stack, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID: env, StackID: stack, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: now,
	}))
	tiles := map[string]store.Tile{}
	for _, slug := range []string{"a", "b"} {
		tl := bootTile(stack, env, slug)
		must(t, st.Tiles.Create(ctx, tl))
		tiles[slug] = tl
		fake.Containers = append(fake.Containers,
			docker.Container{ID: "p" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "pause"}},
			docker.Container{ID: "r" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"}})
	}
	fake.Details = map[string]docker.Detail{
		"pa": {Running: true, Networks: map[string]string{"n": "10.0.0.2"}},
		"ra": {Running: true, Networks: map[string]string{"n": "10.0.0.3"}},
		"pb": {Running: true, Networks: map[string]string{"n": "10.0.0.4"}},
		"rb": {Running: true, Networks: map[string]string{"n": "10.0.0.5"}},
	}
	// A third tile whose pause container is gone is skipped, not fatal.
	gone := bootTile(stack, env, "gone")
	must(t, st.Tiles.Create(ctx, gone))

	must(t, orch.rebuildVIPs(ctx))
	scripts = nil
	must(t, orch.tiles.Route(ctx, tiles["a"], "n"))
	if len(scripts) != 1 {
		t.Fatalf("restore calls = %d, want 1", len(scripts))
	}
	for _, want := range []string{"-d 10.0.0.2/32", "-d 10.0.0.4/32", "--to-destination 10.0.0.5"} {
		if !strings.Contains(scripts[0], want) {
			t.Errorf("script after routing a lacks %q:\n%s", want, scripts[0])
		}
	}
}

func bootTile(stackID, envID, slug string) store.Tile {
	return store.Tile{
		ID: uuid.NewString(), StackID: stackID, EnvironmentID: envID, Name: slug, Slug: slug,
		Kind: tile.Image, ImageRef: "nginx:1", EnvJSON: "{}", BuildArgs: "{}", Replicas: 1,
		UpdatePolicy: "manual", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

// bootRig is a panel with a fresh vip.Table and two routed tiles, a and b.
type bootRig struct {
	orch    *Orchestrator
	fake    *dockerfake.Fake
	table   *vip.Table
	scripts *[]string
	st      *store.Store
	stack   string
	env     string
	tiles   map[string]store.Tile
}

func newBootRig(t *testing.T) bootRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	fake := dockerfake.New()
	scripts := &[]string{}
	table := vip.New()
	table.Restore = func(ctx context.Context, s string) error {
		*scripts = append(*scripts, s)
		return ctx.Err()
	}
	table.Exec = func(context.Context, ...string) error { return nil }
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(fake),
		WithVIP(table),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	st := storetest.Open(t, filepath.Join(dir, "stackr.db"))
	org, stack, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID: env, StackID: stack, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: now,
	}))
	r := bootRig{orch: orch, fake: fake, table: table, scripts: scripts, st: st, stack: stack, env: env, tiles: map[string]store.Tile{}}
	fake.Details = map[string]docker.Detail{}
	fake.Err = map[string]error{}
	for i, slug := range []string{"a", "b"} {
		r.addTile(t, slug, "10.0.0."+string(rune('2'+2*i)), "10.0.0."+string(rune('3'+2*i)))
	}
	*scripts = nil // New's own boot rebuild saw an empty Docker
	return r
}

// addTile creates a tile row, its pause container and one replica.
func (r bootRig) addTile(t *testing.T, slug, pauseIP, replicaIP string) {
	t.Helper()
	tl := bootTile(r.stack, r.env, slug)
	must(t, r.st.Tiles.Create(context.Background(), tl))
	r.tiles[slug] = tl
	r.fake.Containers = append(r.fake.Containers,
		docker.Container{ID: "p" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "pause"}},
		docker.Container{ID: "r" + slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"}})
	r.fake.Details["p"+slug] = docker.Detail{Running: true, Networks: map[string]string{"n": pauseIP}}
	r.fake.Details["r"+slug] = docker.Detail{Running: true, Networks: map[string]string{"n": replicaIP}}
}

// A slow Docker spends the boot context; the rebuild has its own, so a boot
// context that is already done must not fail it.
func TestRebuildVIPsOwnContext(t *testing.T) {
	r := newBootRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	must(t, r.orch.rebuildVIPs(ctx))
	if len(*r.scripts) != 1 || !strings.Contains((*r.scripts)[0], "-d 10.0.0.2/32") {
		t.Fatalf("rebuild under a done boot ctx applied %v", *r.scripts)
	}
}

// A tile skipped for a transient read error must not lose its kernel rules:
// the table keeps its previous entries, nothing is removed.
func TestRebuildVIPsTransientErrorKeepsRules(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	must(t, r.orch.rebuildVIPs(ctx))
	*r.scripts = nil
	r.fake.Err["Inspect"] = errors.New("docker busy")
	_ = r.orch.rebuildVIPs(ctx)
	for _, s := range *r.scripts {
		if strings.Contains(s, "-X ") || !strings.Contains(s, "-d 10.0.0.2/32") || !strings.Contains(s, "-d 10.0.0.4/32") {
			t.Fatalf("rebuild after a transient error dropped rules:\n%s", s)
		}
	}
}

// One tile whose pause container has no address on its env network is skipped
// with its previous rules kept; the other tiles are still rebuilt.
func TestRebuildVIPsUnaddressedPauseSkipsOnlyThatTile(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	must(t, r.orch.rebuildVIPs(ctx))
	*r.scripts = nil
	r.fake.Details["pa"] = docker.Detail{Running: true, Networks: map[string]string{}}
	r.fake.Details["rb"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.7"}}
	_ = r.orch.rebuildVIPs(ctx)
	if len(*r.scripts) != 1 {
		t.Fatalf("restore calls = %d, want 1: %v", len(*r.scripts), *r.scripts)
	}
	s := (*r.scripts)[0]
	for _, want := range []string{"-d 10.0.0.2/32", "--to-destination 10.0.0.3", "-d 10.0.0.4/32", "--to-destination 10.0.0.7"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q (a kept or a rebuilt rule):\n%s", want, s)
		}
	}
	if strings.Contains(s, "-X ") {
		t.Errorf("script removed a chain:\n%s", s)
	}
}

// One tile with a non-IPv4 address must not empty the table for the rest.
func TestRebuildVIPsBadAddressSkipsOnlyThatTile(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	r.addTile(t, "v6", "fd00::2", "fd00::3")
	must(t, r.orch.rebuildVIPs(ctx))
	if len(*r.scripts) != 1 {
		t.Fatalf("restore calls = %d, want 1 (the v6 tile must not fail the rebuild)", len(*r.scripts))
	}
	for _, want := range []string{"-d 10.0.0.2/32", "-d 10.0.0.4/32"} {
		if !strings.Contains((*r.scripts)[0], want) {
			t.Errorf("script lacks %q:\n%s", want, (*r.scripts)[0])
		}
	}
}

// A pause container whose tile row is gone is skipped; the others still apply.
func TestRebuildVIPsPauseWithoutTileRow(t *testing.T) {
	ctx := context.Background()
	r := newBootRig(t)
	r.fake.Containers = append(r.fake.Containers,
		docker.Container{ID: "pz", State: "running", Labels: map[string]string{tile.LabelTile: uuid.NewString(), tile.LabelRole: "pause"}})
	r.fake.Details["pz"] = docker.Detail{Running: true, Networks: map[string]string{"n": "10.0.0.9"}}
	must(t, r.orch.rebuildVIPs(ctx))
	if len(*r.scripts) != 1 || !strings.Contains((*r.scripts)[0], "-d 10.0.0.4/32") {
		t.Fatalf("rebuild with an orphan pause applied %v", *r.scripts)
	}
}

// After a host reboot the boot rebuild may run before containers are up;
// the minute tick re-routes from Docker once they are.
func TestWatchTickRebuildsVIPs(t *testing.T) {
	r := newBootRig(t)
	must(t, r.orch.watchTick(context.Background()))
	if len(*r.scripts) == 0 || !strings.Contains((*r.scripts)[len(*r.scripts)-1], "-d 10.0.0.4/32") {
		t.Fatalf("watchTick did not rebuild the VIP table: %v", *r.scripts)
	}
}
