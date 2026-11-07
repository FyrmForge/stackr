//go:build integration

// Point-in-time restore against a real postgres:17: make test-integration, or
// go test -tags integration -run PITRRestoreAgainstPostgres.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// rig is a source instance "pg" archiving into a local destination, with the
// panel's flows over the real daemon and a store with the rows they read.
type restoreWorld struct {
	t       *testing.T
	d       *docker.Client
	st      *store.Store
	eng     *mflow.Flow
	bak     *fbackup.Flow
	vols    *volume.Leaf
	tiles   *tile.Leaf
	env     store.Environment
	stackID string
	dest    store.BackupDest
	names   []string // containers to remove
	volumes []string // docker volumes to remove
}

func (w *restoreWorld) dk(args ...string) string {
	w.t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		w.t.Fatalf("docker %s: %v\n%s", args[0], err, out)
	}
	return string(out)
}

// psql runs q as user in db inside container name, over TCP so passwords count.
func (w *restoreWorld) psql(name, user, pass, db, q string) (string, error) {
	out, err := exec.Command("docker", "exec", name, "env", "PGPASSWORD="+pass, "psql", "-q", "-tA", "-h", "127.0.0.1",
		"-U", user, "-d", db, "-c", q).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (w *restoreWorld) mustPsql(name, user, pass, db, q string) string {
	w.t.Helper()
	out, err := w.psql(name, user, pass, db, q)
	if err != nil {
		w.t.Fatalf("psql %q: %v\n%s", q, err, out)
	}
	return out
}

func (w *restoreWorld) until(what string, limit time.Duration, f func() bool) {
	w.t.Helper()
	for end := time.Now().Add(limit); !f(); time.Sleep(time.Second) {
		if time.Now().After(end) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// start runs a postgres:17 container for tile t on data (and archive, when
// archive is not empty, with the instance's archiving command).
func (w *restoreWorld) start(t store.Tile, data, archive string) string {
	w.t.Helper()
	def := mflow.Engines["postgres"].Definition()
	name := fmt.Sprintf("stkr-it-%s-%d", t.Slug, time.Now().UnixNano())
	w.names = append(w.names, name)
	args := []string{"run", "-d", "--name", name, "--label", tile.LabelTile + "=" + t.ID, "--label", tile.LabelRole + "=replica",
		"-v", data + ":" + def.Volumes[0], "-e", "POSTGRES_USER=stackr", "-e", "POSTGRES_PASSWORD=admin", "-e", "POSTGRES_DB=postgres"}
	cmd := []string{}
	if archive != "" {
		args = append(args, "-v", archive+":"+def.ArchiveDir)
		cmd = def.ArchiveCmd
	}
	w.dk(append(append(args, def.Image), cmd...)...)
	w.until(name+" to answer", 90*time.Second, func() bool {
		_, err := w.psql(name, "stackr", "admin", "postgres", "SELECT 1")
		return err == nil
	})
	return name
}

func (w *restoreWorld) tile(slug, kind string, edit func(*store.Tile)) store.Tile {
	w.t.Helper()
	row := store.Tile{StackID: w.stackID, EnvironmentID: w.env.ID, Name: slug, Kind: kind}
	if kind == tile.Slice {
		from := "s:dev:pg"
		row.ProvisionFrom = &from
	}
	if kind == tile.Image {
		row.ImageRef = "app:1"
	}
	if edit != nil {
		edit(&row)
	}
	out, err := w.tiles.Create(context.Background(), row)
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

// instance gives managed tile t its engine row, with the admin password the
// container runs under.
func (w *restoreWorld) instance(t store.Tile, pitrDays int) store.ManagedInstance {
	w.t.Helper()
	ctx := context.Background()
	m, err := w.eng.Instances.Create(ctx, t.ID, "postgres", "stackr", "")
	if err != nil {
		w.t.Fatal(err)
	}
	m.AdminPassword, m.PITRDays = "admin", pitrDays
	if err := w.st.ManagedInstances.Update(ctx, m); err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *restoreWorld) volume(slug string, m *store.ManagedInstance) (store.Volume, string) {
	w.t.Helper()
	ctx := context.Background()
	var id *string
	if m != nil {
		id = &m.ID
	}
	v, _, err := w.vols.Declare(ctx, volume.Scope{Kind: "env", ID: w.env.ID}, slug, id)
	if err != nil {
		w.t.Fatal(err)
	}
	name, err := w.vols.Ensure(ctx, v)
	if err != nil {
		w.t.Fatal(err)
	}
	w.volumes = append(w.volumes, name)
	return v, name
}

func newRestoreWorld(t *testing.T) *restoreWorld {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	d, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	// the helper container that unpacks a volume: a stock alpine
	old := docker.ToolImage
	docker.ToolImage = "alpine:3"
	t.Cleanup(func() { docker.ToolImage = old })
	if out, err := exec.Command("docker", "pull", "-q", "alpine:3").CombinedOutput(); err != nil {
		t.Skipf("no alpine:3: %v %s", err, out)
	}
	if out, err := exec.Command("docker", "pull", "-q", mflow.Engines["postgres"].Definition().Image).CombinedOutput(); err != nil {
		t.Skipf("no postgres image: %v %s", err, out)
	}
	ctx := context.Background()
	st := storetest.Store(t)
	w := &restoreWorld{t: t, d: d, st: st}
	w.tiles = tile.New(st.Tiles, d, vipStub{})
	w.vols = volume.New(st.Volumes, d)
	w.eng = &mflow.Flow{
		Tiles:     w.tiles,
		Instances: managed.New(st.ManagedInstances, st.Provisions, st.Bindings),
		Volumes:   w.vols,
		Envs:      environment.New(st.Environments, d),
		Stacks:    stack.New(st.Stacks),
		ReadyWait: time.Minute,
	}
	w.bak = &fbackup.Flow{
		Backups: bk.New(st.BackupDests, st.BackupSchedules, st.BackupRuns),
		Volumes: w.vols,
		Tiles:   w.tiles,
		Scratch: filepath.Join(t.TempDir(), "scratch"),
	}
	dir := t.TempDir()
	if w.dest, err = w.bak.Backups.EnsureLocal(ctx, filepath.Join(dir, "archives")); err != nil {
		t.Fatal(err)
	}
	orgID, now := uuid.NewString(), time.Now()
	w.stackID = uuid.NewString()
	must(t, st.Orgs.Create(ctx, store.Org{ID: orgID, Name: "o", Slug: "o", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: w.stackID, OrgID: orgID, Name: "s", Slug: "s", Settings: "{}", CreatedAt: now}))
	w.env = store.Environment{ID: uuid.NewString(), StackID: w.stackID, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: now}
	must(t, st.Environments.Create(ctx, w.env))
	t.Cleanup(func() {
		for _, n := range w.names {
			_ = exec.Command("docker", "rm", "-f", "-v", n).Run()
		}
		for _, v := range w.volumes {
			_ = exec.Command("docker", "volume", "rm", "-f", v).Run()
		}
	})
	return w
}

// The whole point-in-time path against real postgres: a base backup before
// row A, WAL shipped past row B, a restore to a moment between them. A branch
// copies a slice out of the restored copy (A, not B); a promote puts the
// copy under the source: A and not B, a binding's login still works, a slice
// made after the time is back empty, and the archive carries on into the new
// timeline.
func TestPITRRestoreAgainstPostgres(t *testing.T) {
	w := newRestoreWorld(t)
	ctx := context.Background()
	def := mflow.Engines["postgres"].Definition()
	pgTile := w.tile("pg", tile.Managed, nil)
	pgM := w.instance(pgTile, 7)
	data, dataName := w.volume("pg-data", &pgM)
	_, walName := w.volume("pg-wal", &pgM)
	src := w.start(pgTile, dataName, walName)

	// a slice with an owner and an app binding, both made by the panel's own flow
	appTile := w.tile("app", tile.Slice, nil)
	apiTile := w.tile("api", tile.Image, nil)
	prov, err := w.eng.Provision(ctx, pgTile, appTile)
	must(t, err)
	bind, err := w.eng.Bind(ctx, prov, apiTile, "write")
	must(t, err)
	w.mustPsql(src, prov.DBUser, prov.DBPassword, prov.DBName, `CREATE TABLE t (v text)`)

	// base backup first
	root := bk.PITRRoot("o1", data.ID)
	in := mflow.Instance{AdminUser: "stackr", AdminPassword: "admin", AdminDB: "postgres", PITRDays: 7}
	be := mflow.Engines["postgres"]
	ds, startArgv, stopArgv := be.Backup("base", in), be.(interface{ WalStartArgv(mflow.Instance) []string }).WalStartArgv(in),
		be.(interface{ WalStopArgv(mflow.Instance) []string }).WalStopArgv(in)
	run, err := w.bak.Backup(ctx, fbackup.Subject{Volume: data, Engine: pgTile, Base: true, Dump: ds, WalStart: startArgv, WalStop: stopArgv},
		fbackup.Spec{Dest: w.dest, Prefix: root + "/" + bk.BaseDir, Trigger: "manual", PITRDays: 7}, io.Discard)
	if err != nil {
		t.Fatalf("base backup: %v", err)
	}
	if run.WalStop == nil || bk.BaseWalStart(run.ObjectKey) == "" {
		t.Fatalf("run %+v: no wal_stop, or the key does not name its start", run)
	}

	// A, the moment T1, B; a slice and a binding made after T1
	w.mustPsql(src, prov.DBUser, prov.DBPassword, prov.DBName, `INSERT INTO t VALUES ('A')`)
	time.Sleep(1500 * time.Millisecond)
	t1 := time.Now().UTC()
	time.Sleep(1500 * time.Millisecond)
	w.mustPsql(src, prov.DBUser, prov.DBPassword, prov.DBName, `INSERT INTO t VALUES ('B')`)
	lateTile := w.tile("late", tile.Slice, nil)
	lateProv, err := w.eng.Provision(ctx, pgTile, lateTile)
	must(t, err)
	lateBind, err := w.eng.Bind(ctx, lateProv, apiTile, "write")
	must(t, err)
	w.mustPsql(src, "stackr", "admin", "postgres", `SELECT pg_switch_wal()`)
	ship := fbackup.Ship{Engine: pgTile, Dest: w.dest, Root: root, Dir: def.ArchiveDir}
	var shipped *fbackup.Shipped
	w.until("the WAL to ship", 40*time.Second, func() bool {
		res, err := w.bak.ShipWAL(ctx, ship, io.Discard)
		if err != nil {
			t.Fatalf("ship: %v", err)
		}
		if res.Bundles > 0 {
			shipped = &res
		}
		return shipped != nil && res.Waiting == 0
	})

	// what the restore would read, and the time it takes
	bases, keys, err := w.bak.ListArchive(ctx, w.dest, root)
	must(t, err)
	from, ok := bk.Restorable(bases, keys)
	if !ok || t1.Before(from) {
		t.Fatalf("restorable from %v (%v), T1 = %v", from, ok, t1)
	}
	plan, err := bk.PickBase(bases, keys, t1, false)
	must(t, err)
	if plan.Base.ObjectKey != run.ObjectKey || len(plan.Bundles) == 0 {
		t.Fatalf("plan base %s, %d bundles", plan.Base.ObjectKey, len(plan.Bundles))
	}

	// the restored copy: its own tile, instance and volume, filled before it starts
	candTile := w.tile("pg-restored", tile.Managed, nil)
	candM := w.instance(candTile, 0)
	candData, candName := w.volume("pg-restored-data", &candM)
	conf := be.(interface{ RecoveryConf(time.Time) string }).RecoveryConf(t1)
	if !strings.Contains(conf, "recovery_target_time = '"+t1.Format("2006-01-02 15:04:05.999999")) {
		t.Fatalf("recovery settings do not name T1:\n%s", conf)
	}
	must(t, w.bak.FillVolume(ctx, fbackup.Replay{Dest: w.dest, Plan: plan, Target: candData, WalDir: mflow.PGRestoreWalDir, Conf: conf}, io.Discard))
	cand := w.start(candTile, candName, "")
	w.until("the replay to end", 2*time.Minute, func() bool {
		rec, err := w.eng.Recovering(ctx, candTile)
		return err == nil && !rec
	})
	must(t, w.eng.Settle(ctx, candTile))
	if out, err := exec.Command("docker", "exec", cand, "ls", def.Volumes[0]+"/"+mflow.PGRestoreWalDir).CombinedOutput(); err == nil {
		t.Errorf("the WAL copy stayed after settling: %s", out)
	}
	if got := w.mustPsql(cand, "stackr", "admin", "postgres", `SELECT count(*) FROM pg_settings WHERE name = 'restore_command' AND source = 'configuration file'`); got != "0" {
		t.Errorf("restore_command is still in the configuration: %s", got)
	}
	count := func(name, user, pass, db, where string) string {
		return w.mustPsql(name, user, pass, db, `SELECT count(*) FROM t WHERE v = '`+where+`'`)
	}
	if a, b := count(cand, prov.DBUser, prov.DBPassword, prov.DBName, "A"), count(cand, prov.DBUser, prov.DBPassword, prov.DBName, "B"); a != "1" || b != "0" {
		t.Fatalf("restored copy has A=%s B=%s, want 1 and 0", a, b)
	}
	if got := w.mustPsql(src, prov.DBUser, prov.DBPassword, prov.DBName, `SELECT count(*) FROM t`); got != "2" {
		t.Fatalf("the source was touched: %s rows", got)
	}

	// (a) branch --at T1: the slice's database out of the copy into a new slice of the source
	brTile := w.tile("app-br-x1y2", tile.Slice, nil)
	brProv, err := w.eng.Provision(ctx, pgTile, brTile)
	must(t, err)
	out, wait, err := w.eng.DumpSliceOn(ctx, appTile, candTile)
	must(t, err)
	must(t, errors.Join(w.eng.Seed(ctx, brTile, mflow.DumpCustom, out, io.Discard), wait()))
	if a, b := count(src, brProv.DBUser, brProv.DBPassword, brProv.DBName, "A"), count(src, brProv.DBUser, brProv.DBPassword, brProv.DBName, "B"); a != "1" || b != "0" {
		t.Errorf("branch has A=%s B=%s, want 1 and 0", a, b)
	}

	// (b) promote: history to the archive, the source stops, its volume row takes the copy's data
	must(t, w.bak.ShipHistory(ctx, fbackup.Ship{Engine: candTile, Dest: w.dest, Root: root, Dir: mflow.PGWalDir}, io.Discard))
	_, wal, err := w.bak.ListArchive(ctx, w.dest, root)
	must(t, err)
	sawHistory := false
	for _, k := range wal {
		if _, hist, ok := bk.BundleLast(k); ok && hist {
			sawHistory = true
		}
	}
	if !sawHistory {
		t.Fatalf("no timeline history reached the archive: %v", wal)
	}
	w.dk("stop", cand)
	w.dk("rm", "-f", src) // volumes stay
	o := &Orchestrator{store: w.st, volumes: w.vols}
	sv, _ := w.vols.Get(ctx, data.ID)
	cv, _ := w.vols.Get(ctx, candData.ID)
	must(t, o.swapData(ctx, sv, cv))
	sv, _ = w.vols.Get(ctx, data.ID)
	src2 := w.start(pgTile, sv.Name, walName)
	must(t, w.eng.Replay(ctx, pgTile, io.Discard))

	if a, b := count(src2, prov.DBUser, prov.DBPassword, prov.DBName, "A"), count(src2, prov.DBUser, prov.DBPassword, prov.DBName, "B"); a != "1" || b != "0" {
		t.Errorf("promoted source has A=%s B=%s, want 1 and 0", a, b)
	}
	// the app's own login still connects: the binding row's user and password are the ones in the data
	if got, err := w.psql(src2, bind.DBUser, bind.DBPassword, prov.DBName, `SELECT count(*) FROM t`); err != nil || got != "1" {
		t.Errorf("binding login: %q %v", got, err)
	}
	// the slice made after T1 is back, empty, and its binding works
	if got, err := w.psql(src2, lateBind.DBUser, lateBind.DBPassword, lateProv.DBName, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`); err != nil || got != "0" {
		t.Errorf("late slice: %q %v", got, err)
	}
	if got := w.mustPsql(src2, "stackr", "admin", "postgres", `SELECT pg_is_in_recovery()`); got != "f" {
		t.Errorf("pg_is_in_recovery = %s", got)
	}
	if got := w.mustPsql(src2, "stackr", "admin", "postgres", `SELECT current_setting('archive_mode')`); got != "on" {
		t.Errorf("archive_mode = %s", got)
	}

	// the archive carries on: new WAL ships under the new timeline
	w.mustPsql(src2, prov.DBUser, prov.DBPassword, prov.DBName, `INSERT INTO t VALUES ('C')`)
	w.mustPsql(src2, "stackr", "admin", "postgres", `SELECT pg_switch_wal()`)
	var last string
	w.until("the new timeline's WAL to ship", 40*time.Second, func() bool {
		res, err := w.bak.ShipWAL(ctx, ship, io.Discard)
		if err != nil {
			t.Fatalf("ship after promote: %v", err)
		}
		if res.Last != "" {
			last = res.Last
		}
		return strings.HasPrefix(last, "00000002")
	})
}
