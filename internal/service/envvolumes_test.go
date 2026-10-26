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
	v, adopted, err := w.orch.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	if adopted {
		t.Error("a new volume reads as adopted")
	}
	if _, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "data"); !invalidOn(err, "volume", "already has") {
		t.Errorf("duplicate = %v", err)
	}
	if _, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "Bad Name"); !invalidOn(err, "volume", "lower-case") {
		t.Errorf("bad name = %v", err)
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
