package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The env's Volumes row: declare, a second of the same name refused, a
// tile may mount only a declared one, remove refused while mounted (the
// tile named), then orphaned and gone from the list, and declaring it
// again takes the data back.
func TestEnvVolumes(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", false)

	if _, _, err := w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error { t.Volumes = "data:/data"; return nil }); !invalidOn(err, "volumes", "does not declare data") {
		t.Fatalf("undeclared mount = %v", err)
	}
	if _, err := w.orch.CreateTile(ctx, Tile{StackID: w.stack, EnvironmentID: w.env, Name: "web", Kind: "image", ImageRef: "nginx:1", Volumes: "data:/data"}); !invalidOn(err, "volumes", "does not declare data") {
		t.Fatalf("create with an undeclared mount = %v", err)
	}
	v, adopted, err := w.orch.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	if adopted {
		t.Error("a new volume reads as adopted")
	}
	if _, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "data"); !invalidOn(err, "volume", "already has") {
		t.Errorf("duplicate = %v", err)
	}
	for name, says := range map[string]string{"Bad Name": "Use lower-case", "": "Enter a name.", strings.Repeat("a", 41): "40 characters"} {
		if _, _, err := w.orch.DeclareEnvVolume(ctx, w.env, name); !invalidOn(err, "volume", says) {
			t.Errorf("name %q = %v", name, err)
		}
	}
	// a share the org does not have is refused like an undeclared volume,
	// on its own line
	_, _, err = w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error { t.Volumes = "data:/data\nshare:nope/x:/s"; return nil })
	if bads := errs.Invalids(err); len(bads) != 1 || bads[0].Line != 2 || !strings.Contains(bads[0].Msg, "no share nope") {
		t.Errorf("missing share = %+v", bads)
	}
	_, _, err = w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error { t.Volumes = "data:/data:ro"; return nil })
	must(t, err)

	vs, err := w.orch.ListEnvVolumes(ctx, w.env)
	must(t, err)
	if len(vs.Volumes) != 1 || vs.Managed || strings.Join(vs.Volumes[0].MountedBy, ",") != "api" {
		t.Fatalf("list = %+v", vs)
	}
	if err := w.orch.RemoveEnvVolume(ctx, w.env, v.ID); err == nil || !strings.Contains(err.Error(), "mounted by api") {
		t.Fatalf("remove in use = %v", err)
	}
	if err := w.orch.RemoveEnvVolume(ctx, "other-env", v.ID); err == nil {
		t.Error("removed through another env")
	}

	_, _, err = w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error { t.Volumes = ""; return nil })
	must(t, err)
	must(t, w.orch.RemoveEnvVolume(ctx, w.env, v.ID))
	if vs, _ := w.orch.ListEnvVolumes(ctx, w.env); len(vs.Volumes) != 0 {
		t.Errorf("orphan still listed: %+v", vs.Volumes)
	}
	if got, err := w.orch.Volume(ctx, v.ID); err != nil || got.OrphanedAt == nil {
		t.Errorf("removed volume = %+v %v, want kept and orphaned", got, err)
	}
	// an orphan is no longer declared for a tile
	if _, _, err := w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error { t.Volumes = "data:/data"; return nil }); !invalidOn(err, "volumes", "data") {
		t.Errorf("orphan mount = %v", err)
	}
	back, adopted, err := w.orch.DeclareEnvVolume(ctx, w.env, "data")
	if err != nil || !adopted || back.ID != v.ID {
		t.Errorf("re-declare = %+v %v %v, want the orphan adopted", back, adopted, err)
	}
	// a tile whose delete is queued no longer holds the volume
	_, _, err = w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error { t.Volumes = "data:/data"; return nil })
	must(t, err)
	_, err = w.orch.DeleteTile(ctx, api.ID)
	must(t, err)
	if ts, err := w.orch.Tiles(ctx, w.env); err != nil || len(ts) != 1 {
		t.Fatalf("tile gone before its delete ran: %d %v", len(ts), err)
	}
	if err := w.orch.RemoveEnvVolume(ctx, w.env, v.ID); err != nil {
		t.Errorf("remove after the tile's delete = %v", err)
	}
}

// A managed instance's volume is listed as its own, never removable; a
// config-managed stack's env volumes are its file's.
func TestEnvVolumesOwnedAndManaged(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, err := w.orch.CreateManagedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	ms, err := w.orch.ManagedInstances(ctx, w.env)
	must(t, err)
	must(t, w.st.Volumes.Create(ctx, store.Volume{ID: "v-pg", ScopeKind: "env", ScopeID: w.env, InstanceID: &ms[0].ID, Slug: "pgdata", Name: "stackr-vol-v-pg", CreatedAt: time.Now()}))
	vs, err := w.orch.ListEnvVolumes(ctx, w.env)
	must(t, err)
	if len(vs.Volumes) != 1 || vs.Volumes[0].OwnedBy != "pg" {
		t.Fatalf("instance volume = %+v", vs.Volumes)
	}
	if err := w.orch.RemoveEnvVolume(ctx, w.env, "v-pg"); err == nil {
		t.Error("removed an instance's volume")
	}
	if _, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "pgdata"); err == nil {
		t.Error("declare took over an instance's volume")
	}
	if ms, _ := w.orch.MountableVolumes(ctx, w.env); len(ms) != 0 {
		t.Errorf("mountable = %v, want the instance's left out", ms)
	}

	st, err := w.st.Stacks.Get(ctx, w.stack)
	must(t, err)
	st.ConfigRepo = "https://github.com/acme/shop"
	must(t, w.st.Stacks.Update(ctx, st))
	if _, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "uploads"); !conflict(err) {
		t.Errorf("declare on a config-managed stack = %v", err)
	}
	if vs, _ := w.orch.ListEnvVolumes(ctx, w.env); !vs.Managed {
		t.Error("config-managed not flagged")
	}
}

func invalidOn(err error, field, words string) bool {
	for _, bad := range errs.Invalids(err) {
		if bad.Field == field && strings.Contains(bad.Msg, words) {
			return true
		}
	}
	return false
}

func conflict(err error) bool {
	_, ok := errs.IsConflict(err)
	return ok && !errors.Is(err, errs.ErrNotFound)
}
