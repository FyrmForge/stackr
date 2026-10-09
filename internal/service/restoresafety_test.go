package service

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// pg is a managed postgres tile with its running container and data volume.
func (w *world) pg(t *testing.T, name string) (Tile, store.ManagedInstance, store.Volume) {
	t.Helper()
	ctx := context.Background()
	tl, err := w.orch.CreateManagedTile(ctx, Tile{EnvironmentID: w.env, Name: name}, "postgres")
	must(t, err)
	m, err := w.st.ManagedInstances.GetByTile(ctx, tl.ID)
	must(t, err)
	v, _, err := w.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: w.env}, name+"-data", 0, &m.ID)
	must(t, err)
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID:     "c-" + name,
		State:  "running",
		Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
	})
	return tl, m, v
}

// dumpOf takes a whole-instance dump of v's instance, as pg_dumpall's would
// open.
func (w *world) dumpOf(t *testing.T, v store.Volume) store.BackupRun {
	t.Helper()
	ctx := context.Background()
	it, err := w.orch.instanceTile(ctx, *v.InstanceID)
	must(t, err)
	marker, err := w.orch.engines.DumpMarker(ctx, it)
	must(t, err)
	w.fake.ExecOut = marker + "\nSELECT 1;\n"
	sub, err := w.orch.subject(ctx, v, "dump")
	must(t, err)
	local, err := w.orch.localDest(ctx)
	must(t, err)
	r, err := w.orch.backup.Backup(ctx, sub, fbackup.Spec{
		Dest:    local,
		Prefix:  backup.Prefix(w.org, v.ID, ""),
		Trigger: "manual",
	}, io.Discard)
	must(t, err)
	return r
}

func (w *world) restore(src, dst store.Volume, run store.BackupRun) error {
	return w.orch.runRestore(context.Background(), &jobs.Run{Log: io.Discard}, restoreJob{
		RunID:          run.ID,
		SourceVolumeID: src.ID,
		TargetVolumeID: dst.ID,
	})
}

// moves are the container calls a restore made, as "Method id".
func moves(w *world, from int) []string {
	var out []string
	for _, c := range w.fake.Calls()[from:] {
		switch c.Method {
		case "Stop", "Start", "ExecStream":
			out = append(out, c.Method+" "+c.Args[0])
		}
	}
	return out
}

// A cluster dump restored onto another instance drops that instance's
// slices and loads names it does not track: refused, nothing touched.
func TestClusterRestoreAcrossInstancesIsRefused(t *testing.T) {
	w := newWorld(t)
	_, _, a := w.pg(t, "pga")
	_, _, b := w.pg(t, "pgb")
	run := w.dumpOf(t, a)
	before := len(w.fake.Calls())
	err := w.restore(a, b, run)
	if err == nil || !strings.Contains(err.Error(), "onto the instance it came from") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if got := moves(w, before); len(got) != 0 {
		t.Errorf("a refused restore touched containers: %v", got)
	}
}

// A slice or binding made after the backup is not in the dump; the restore
// would drop it. Refused with the list.
func TestClusterRestoreRefusesNewerSlicesAndBindings(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, m, v := w.pg(t, "pg")
	slice := w.tile(t, "ordersdb", false)
	api := w.tile(t, "api", true)
	run := w.dumpOf(t, v)
	later := time.Now().Add(time.Minute)
	p := store.Provision{
		ID: uuid.NewString(), TileID: slice.ID, InstanceID: m.ID,
		DBName: "shop_dev_orders", DBUser: "shop_dev_orders", CreatedAt: later,
	}
	must(t, w.st.Provisions.Create(ctx, p))
	before := len(w.fake.Calls())
	err := w.restore(v, v, run)
	if err == nil || !strings.Contains(err.Error(), "shop_dev_orders") {
		t.Fatalf("a newer slice: err = %v, want a refusal naming it", err)
	}
	if got := moves(w, before); len(got) != 0 {
		t.Errorf("a refused restore touched containers: %v", got)
	}

	// the slice is older than the run, its binding is not
	must(t, w.st.Provisions.Delete(ctx, p.ID))
	p.CreatedAt = time.Now().Add(-time.Hour)
	must(t, w.st.Provisions.Create(ctx, p))
	must(t, w.st.Bindings.Create(ctx, store.Binding{
		ID: uuid.NewString(), ProvisionID: p.ID, ConsumerTileID: api.ID, Access: "write",
		DBUser: "shop_dev_orders_api", Outputs: "{}", CreatedAt: later,
	}))
	err = w.restore(v, v, run)
	if err == nil || !strings.Contains(err.Error(), "shop_dev_orders_api") {
		t.Fatalf("a newer binding: err = %v, want a refusal naming it", err)
	}
}

// The tiles bound to the instance are stopped for the wipe and load, and
// started after.
func TestClusterRestoreStopsBoundTiles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, m, v := w.pg(t, "pg")
	slice := w.tile(t, "ordersdb", false)
	api := w.tile(t, "api", true)
	p := store.Provision{
		ID: uuid.NewString(), TileID: slice.ID, InstanceID: m.ID,
		DBName: "shop_dev_orders", DBUser: "shop_dev_orders", CreatedAt: time.Now().Add(-time.Hour),
	}
	must(t, w.st.Provisions.Create(ctx, p))
	must(t, w.st.Bindings.Create(ctx, store.Binding{
		ID: uuid.NewString(), ProvisionID: p.ID, ConsumerTileID: api.ID, Access: "write",
		DBUser: "shop_dev_orders_api", Outputs: "{}", CreatedAt: time.Now().Add(-time.Hour),
	}))
	run := w.dumpOf(t, v)
	before := len(w.fake.Calls())
	must(t, w.restore(v, v, run))
	want := []string{"ExecStream c-pg", "Stop c-api", "ExecStream c-pg", "Start c-api"}
	if got := moves(w, before); !slices.Equal(got, want) {
		t.Errorf("restore = %v, want %v", got, want)
	}
}

// A restore and a deploy that provisions or binds on the instance must not
// overlap the load: the restore job holds the instance tile's lock, and the
// slice and consumer tiles' too (a single-tile deploy locks only its own id).
func TestRestoreLocksTheInstanceTile(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tl, m, v := w.pg(t, "pg")
	slice := w.tile(t, "ordersdb", false)
	api := w.tile(t, "api", true)
	p := store.Provision{
		ID: uuid.NewString(), TileID: slice.ID, InstanceID: m.ID,
		DBName: "shop_dev_orders", DBUser: "shop_dev_orders", CreatedAt: time.Now(),
	}
	must(t, w.st.Provisions.Create(ctx, p))
	must(t, w.st.Bindings.Create(ctx, store.Binding{
		ID: uuid.NewString(), ProvisionID: p.ID, ConsumerTileID: api.ID, Access: "write",
		DBUser: "shop_dev_orders_api", Outputs: "{}", CreatedAt: time.Now(),
	}))
	j, err := w.orch.RestoreBackup(ctx, nil, "no-such-run", v.ID, v.ID)
	must(t, err)
	for _, want := range []string{tl.ID, slice.ID, api.ID} {
		if !slices.Contains(j.LockSet, want) {
			t.Errorf("lock set = %v, want it to hold %s", j.LockSet, want)
		}
	}
}

// A stack key restores only inside its stack (404 otherwise); other callers keep the org-wide rule, and a job carries its stack_id.
func TestRestoreStaysInTheStack(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	mk := func(kind, scope, slug string) store.Volume {
		v := store.Volume{ID: uuid.NewString(), ScopeKind: kind, ScopeID: scope, Slug: slug, Name: slug, CreatedAt: time.Now()}
		must(t, w.st.Volumes.Create(ctx, v))
		return v
	}
	st, err := w.orch.CreateStack(ctx, w.org, "other", "")
	must(t, err)
	a, a2, b, o := mk("stack", w.stack, "a"), mk("stack", w.stack, "a2"), mk("stack", st.ID, "b"), mk("org", w.org, "o")
	key := &Principal{Access: authz.User{KeyStack: w.stack}}
	j, err := w.orch.RestoreBackup(ctx, key, "r", a.ID, a2.ID)
	must(t, err)
	if !strings.Contains(j.Payload, `"stack_id":"`+w.stack+`"`) {
		t.Errorf("payload = %s, want stack_id %s", j.Payload, w.stack)
	}
	for _, c := range [][2]string{{a.ID, b.ID}, {a.ID, o.ID}, {b.ID, a.ID}, {o.ID, a.ID}} {
		if _, err := w.orch.RestoreBackup(ctx, key, "r", c[0], c[1]); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("stack key restore %s into %s = %v, want not found", c[0], c[1], err)
		}
		if _, err := w.orch.RestoreBackup(ctx, nil, "r", c[0], c[1]); err != nil {
			t.Errorf("session restore %s into %s = %v, want allowed", c[0], c[1], err)
		}
	}
}
