package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

var ctx = context.Background()

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string, []string) error {
	return nil
}

func (vipStub) Remove(context.Context, string) error {
	return nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type world struct {
	f    *Flow
	fake *dockerfake.Fake
	dest store.BackupDest
	vol  store.Volume
	api  store.Tile
}

// setup: a volume "data" mounted by tile "api" (one running replica "r1"),
// and the install's local destination.
func setup(t *testing.T) *world {
	s := storetest.Store(t)
	fake := dockerfake.New()
	dir := t.TempDir()
	f := &Flow{
		Backups: bk.New(s.BackupDests, s.BackupSchedules, s.BackupRuns),
		Volumes: volume.New(s.Volumes, fake),
		Tiles:   tile.New(s.Tiles, fake, vipStub{}),
		Scratch: filepath.Join(dir, "scratch"),
	}
	dest, err := f.Backups.EnsureLocal(ctx, filepath.Join(dir, "archives"))
	must(t, err)
	v, _, err := f.Volumes.Declare(ctx, volume.Scope{Kind: "env", ID: "e1"}, "data", 0, nil)
	must(t, err)
	api := store.Tile{ID: "t1", Slug: "api"}
	fake.Containers = []docker.Container{{
		ID:     "r1",
		State:  "running",
		Labels: map[string]string{tile.LabelTile: "t1", tile.LabelRole: "replica"},
	}}
	fake.TarOut = tarGz(t, "hello.txt", "hi")
	return &world{
		f:    f,
		fake: fake,
		dest: dest,
		vol:  v,
		api:  api,
	}
}

func tarGz(t *testing.T, name, body string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	must(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}))
	_, err := tw.Write([]byte(body))
	must(t, err)
	must(t, tw.Close())
	must(t, gz.Close())
	return b.Bytes()
}

func (w *world) spec(keep int) Spec {
	return Spec{
		Dest:    w.dest,
		Prefix:  bk.Prefix("o1", w.vol.ID, "s1"),
		Trigger: "schedule",
		Keep:    keep,
	}
}

func order(f *dockerfake.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		switch c.Method {
		case "Pause", "Unpause", "Stop", "Start", "TarVolume", "UntarVolume", "ExecStream":
			out = append(out, c.Method)
		}
	}
	return out
}

// Local round trip: pause, tar, thaw; the stored archive is encrypted; the
// restore backs the target up first, verifies, stops, untars, starts.
func TestLocalRoundTripAndRestoreOrder(t *testing.T) {
	w := setup(t)
	sub := Subject{Volume: w.vol, Holders: []store.Tile{w.api}}
	r, err := w.f.Backup(ctx, sub, w.spec(0), io.Discard)
	must(t, err)
	if r.Status != bk.Done || r.ObjectKey == "" {
		t.Fatalf("run = %+v", r)
	}
	if got := order(w.fake); !slices.Equal(got, []string{"Pause", "TarVolume", "Unpause"}) {
		t.Errorf("backup order = %v", got)
	}
	stored, err := os.ReadFile(filepath.Join(w.dest.Endpoint, r.ObjectKey))
	must(t, err)
	if bytes.Contains(stored, []byte("hello.txt")) || bytes.HasPrefix(stored, []byte{0x1f, 0x8b}) {
		t.Error("the archive is stored in the clear")
	}

	before := len(w.fake.Calls())
	must(t, w.f.Restore(ctx, Restore{
		RunID:          r.ID,
		SourceVolumeID: w.vol.ID,
		Target:         sub,
		Pre:            Spec{Dest: w.dest, Prefix: bk.Prefix("o1", w.vol.ID, "")},
	}, io.Discard))
	var got []string
	for _, c := range w.fake.Calls()[before:] {
		switch c.Method {
		case "Pause", "Unpause", "Stop", "Start", "TarVolume", "UntarVolume":
			got = append(got, c.Method)
		}
	}
	if want := []string{"Pause", "TarVolume", "Unpause", "Stop", "UntarVolume", "Start"}; !slices.Equal(got, want) {
		t.Errorf("restore order = %v, want %v", got, want)
	}
	if !bytes.Equal(w.fake.Untarred, w.fake.TarOut) {
		t.Error("the restored bytes are not the archive")
	}
	runs, err := w.f.Backups.Runs(ctx, w.vol.ID)
	must(t, err)
	if len(runs) != 2 || runs[0].Trigger != bk.PreRestore {
		t.Errorf("runs = %+v", runs)
	}
}

// A corrupt archive fails verification before anything is stopped or wiped.
func TestCorruptArchiveTouchesNothing(t *testing.T) {
	w := setup(t)
	w.fake.TarOut = []byte("not a tarball")
	sub := Subject{Volume: w.vol, Holders: []store.Tile{w.api}}
	r, err := w.f.Backup(ctx, sub, w.spec(0), io.Discard)
	must(t, err)
	w.fake.TarOut = tarGz(t, "x", "y") // the pre-restore backup is fine
	before := len(w.fake.Calls())
	err = w.f.Restore(ctx, Restore{
		RunID:          r.ID,
		SourceVolumeID: w.vol.ID,
		Target:         sub,
		Pre:            Spec{Dest: w.dest, Prefix: bk.Prefix("o1", w.vol.ID, "")},
	}, io.Discard)
	if err == nil {
		t.Fatal("a corrupt archive restored")
	}
	for _, c := range w.fake.Calls()[before:] {
		if c.Method == "Stop" || c.Method == "UntarVolume" {
			t.Errorf("%s ran before the archive was verified", c.Method)
		}
	}
}

func TestDumpRoundTripAndPrune(t *testing.T) {
	w := setup(t)
	w.fake.ExecOut = "CREATE TABLE t();"
	sub := Subject{
		Volume: w.vol,
		Engine: w.api,
		Dump:   []string{"pg_dump"},
		Load:   []string{"psql"},
	}
	_, err := w.f.Backup(ctx, sub, w.spec(1), io.Discard)
	must(t, err)
	r, err := w.f.Backup(ctx, sub, w.spec(1), io.Discard)
	must(t, err)
	keys, err := open(w.dest).List(ctx, w.spec(1).Prefix+"/")
	must(t, err)
	if len(keys) != 1 || keys[0] != r.ObjectKey {
		t.Errorf("after prune: %v", keys)
	}
	must(t, w.f.Restore(ctx, Restore{
		RunID:          r.ID,
		SourceVolumeID: w.vol.ID,
		Target:         sub,
		Pre:            Spec{Dest: w.dest, Prefix: bk.Prefix("o1", w.vol.ID, "")},
	}, io.Discard))
	if string(w.fake.ExecIn) != "CREATE TABLE t();" {
		t.Errorf("psql got %q", w.fake.ExecIn)
	}
	// A dump never restores into a plain volume.
	if err := w.f.Restore(ctx, Restore{
		RunID:          r.ID,
		SourceVolumeID: w.vol.ID,
		Target:         Subject{Volume: w.vol},
		Pre:            w.spec(0),
	}, io.Discard); err == nil {
		t.Error("a dump restored as a tarball")
	}
}

func TestPanelArchive(t *testing.T) {
	w := setup(t)
	p := Panel{
		MasterKey:  "mk",
		Version:    "v1.2.3",
		Passphrase: "correct horse",
		InstallID:  "i1",
		Vacuum:     func(_ context.Context, path string) error { return os.WriteFile(path, []byte("sqlite"), 0o600) },
	}
	r, err := w.f.PanelBackup(ctx, p, w.dest, 0, io.Discard)
	must(t, err)
	f, err := os.Open(filepath.Join(w.dest.Endpoint, r.ObjectKey))
	must(t, err)
	defer func() { _ = f.Close() }()
	dec, err := decrypt(f, "correct horse")
	must(t, err)
	gz, err := gzip.NewReader(dec)
	must(t, err)
	tr := tar.NewReader(gz)
	var names []string
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		names = append(names, h.Name)
	}
	if !slices.Equal(names, []string{"stackr.db", "keys/master.key", "VERSION"}) {
		t.Errorf("members = %v", names)
	}
	p.MasterKey = ""
	if _, err := w.f.PanelBackup(ctx, p, w.dest, 0, io.Discard); err == nil {
		t.Error("a panel backup without the master key succeeded")
	}
}

// The panel archive carries the proxy's volume tree as caddy.tar.gz; a
// scheduled run is marked so; a proxy volume that cannot be read is logged
// and the archive still lands.
func TestPanelArchiveCaddy(t *testing.T) {
	w := setup(t)
	p := Panel{
		MasterKey:  "mk",
		Version:    "v1.2.3",
		Passphrase: "pw",
		InstallID:  "i1",
		Trigger:    "schedule",
		Vacuum:     func(_ context.Context, path string) error { return os.WriteFile(path, []byte("sqlite"), 0o600) },
		Caddy: func(_ context.Context, out io.Writer) error {
			_, err := out.Write(tarGz(t, "data/certs/a.crt", "pem"))
			return err
		},
	}
	r, err := w.f.PanelBackup(ctx, p, w.dest, 0, io.Discard)
	must(t, err)
	if r.Trigger != "schedule" {
		t.Errorf("trigger = %q", r.Trigger)
	}
	got := panelMembers(t, filepath.Join(w.dest.Endpoint, r.ObjectKey), "pw")
	if !slices.Equal(got.names, []string{"stackr.db", "keys/master.key", "VERSION", "caddy.tar.gz"}) {
		t.Fatalf("members = %v", got.names)
	}
	gz, err := gzip.NewReader(bytes.NewReader(got.bodies["caddy.tar.gz"]))
	must(t, err)
	h, err := tar.NewReader(gz).Next()
	if err != nil || h.Name != "data/certs/a.crt" {
		t.Errorf("caddy tree = %v %v", h, err)
	}

	p.Caddy = func(context.Context, io.Writer) error { return errors.New("no such volume") }
	var log bytes.Buffer
	r, err = w.f.PanelBackup(ctx, p, w.dest, 0, &log)
	must(t, err)
	if got := panelMembers(t, filepath.Join(w.dest.Endpoint, r.ObjectKey), "pw"); slices.Contains(got.names, "caddy.tar.gz") {
		t.Errorf("a failed proxy tar was archived: %v", got.names)
	}
	if !strings.Contains(log.String(), "proxy volume not archived") {
		t.Errorf("log = %q", log.String())
	}
}

type members struct {
	names  []string
	bodies map[string][]byte
}

func panelMembers(t *testing.T, path, pass string) members {
	t.Helper()
	f, err := os.Open(path)
	must(t, err)
	defer func() { _ = f.Close() }()
	dec, err := decrypt(f, pass)
	must(t, err)
	gz, err := gzip.NewReader(dec)
	must(t, err)
	m := members{bodies: map[string][]byte{}}
	tr := tar.NewReader(gz)
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		m.names = append(m.names, h.Name)
		m.bodies[h.Name], _ = io.ReadAll(tr)
	}
	return m
}

// The orphan job archives an expired orphan that holds data, then deletes it.
func TestOrphans(t *testing.T) {
	w := setup(t)
	w.fake.Volumes = []docker.VolumeInfo{{Name: w.vol.Name, SizeBytes: 10}}
	_, err := w.f.Volumes.Orphan(ctx, w.vol)
	must(t, err)
	must(t, w.f.Orphans(ctx, 0, w.dest, io.Discard))
	keys, err := open(w.dest).List(ctx, bk.OrphanPrefix(w.vol.ID)+"/")
	must(t, err)
	if len(keys) != 1 {
		t.Errorf("orphan archives = %v", keys)
	}
	if _, err := w.f.Volumes.Get(ctx, w.vol.ID); err == nil {
		t.Error("the orphan survived")
	}
}

func panelFor(trigger string) Panel {
	return Panel{
		MasterKey:  "mk",
		Version:    "v1",
		Passphrase: "pw",
		InstallID:  "i1",
		Trigger:    trigger,
		Vacuum:     func(_ context.Context, path string) error { return os.WriteFile(path, []byte("sqlite"), 0o600) },
	}
}

// Archives go under a prefix per trigger: a scheduled run pruned to 1 leaves
// the pre-upgrade archive alone, and the new archive survives its own prune
// even when older-dated ones fill the prefix.
func TestPanelPruneByTrigger(t *testing.T) {
	w := setup(t)
	up, err := w.f.PanelBackup(ctx, panelFor("upgrade"), w.dest, 14, io.Discard)
	must(t, err)
	sched := bk.PanelPrefix("i1", "schedule")
	future := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 2 { // dated ahead: sort after anything written now
		k := bk.ObjectKey(sched, "panel.tar.gz.age", future.AddDate(0, 0, i))
		must(t, open(w.dest).Put(ctx, k, strings.NewReader("old")))
	}
	r, err := w.f.PanelBackup(ctx, panelFor("schedule"), w.dest, 2, io.Discard)
	must(t, err)
	keys, err := open(w.dest).List(ctx, sched+"/")
	must(t, err)
	if !slices.Contains(keys, r.ObjectKey) {
		t.Errorf("the archive just written was pruned: %v", keys)
	}
	if !strings.HasPrefix(r.ObjectKey, sched+"/") || strings.HasPrefix(up.ObjectKey, sched+"/") {
		t.Errorf("keys not split by trigger: %s, %s", r.ObjectKey, up.ObjectKey)
	}
	if left, _ := open(w.dest).List(ctx, bk.PanelPrefix("i1", "upgrade")+"/"); len(left) != 1 {
		t.Errorf("upgrade archive after a scheduled prune: %v", left)
	}
}

const marker = "-- test dump format: cluster"

// failNthStream makes the nth ExecStream fail, every other call passes.
type failNthStream struct {
	tile.Docker
	n, at int
}

func (d *failNthStream) ExecStream(
	ctx context.Context,
	id string,
	cmd []string,
	in io.Reader,
) (io.Reader, func() error, error) {
	d.n++
	if d.n == d.at {
		return nil, nil, errors.New("load died")
	}
	return d.Docker.ExecStream(ctx, id, cmd, in)
}

// dumpWorld: engine tile "db" (container r2) with consumer "api" (r1), and a
// cluster dump of it taken. at > 0 makes that ExecStream call fail.
func dumpWorld(t *testing.T, at int) (*world, Subject, store.BackupRun) {
	w := setup(t)
	if at > 0 {
		s := storetest.Store(t)
		w.f.Tiles = tile.New(s.Tiles, &failNthStream{Docker: w.fake, at: at}, vipStub{})
	}
	db := store.Tile{ID: "t2", Slug: "db"}
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID:     "r2",
		State:  "running",
		Labels: map[string]string{tile.LabelTile: "t2", tile.LabelRole: "replica"},
	})
	w.fake.ExecOut = marker + "\nSELECT 1;\n"
	sub := Subject{
		Volume:    w.vol,
		Engine:    db,
		Dump:      []string{"pg_dumpall"},
		Load:      []string{"psql"},
		Consumers: []store.Tile{w.api},
		Marker:    marker,
	}
	r, err := w.f.Backup(ctx, sub, w.spec(0), io.Discard)
	must(t, err)
	return w, sub, r
}

func (w *world) restore(sub Subject, r store.BackupRun, cluster func(context.Context) error) error {
	return w.f.Restore(ctx, Restore{
		RunID:          r.ID,
		SourceVolumeID: w.vol.ID,
		Target:         sub,
		Pre:            Spec{Dest: w.dest, Prefix: bk.Prefix("o1", w.vol.ID, "")},
		Cluster:        cluster,
	}, io.Discard)
}

// Apps writing during the load break a restore after the wipe (duplicate
// keys, a PK never built): the consumers are stopped first and started after.
func TestClusterRestoreStopsConsumers(t *testing.T) {
	w, sub, r := dumpWorld(t, 0)
	before := len(w.fake.Calls())
	must(t, w.restore(sub, r, nil))
	var got []string
	for _, c := range w.fake.Calls()[before:] {
		switch c.Method {
		case "Stop", "Start", "ExecStream":
			got = append(got, c.Method+" "+c.Args[0])
		}
	}
	want := []string{"ExecStream r2", "Stop r1", "ExecStream r2", "Start r1"}
	if !slices.Equal(got, want) {
		t.Errorf("restore order = %v, want %v (pre-restore dump, stop consumers, load, start)", got, want)
	}
}

// A load that dies still brings the consumers back, and the error names the
// pre-restore run to restore from.
func TestClusterRestoreFailureStartsConsumersAndNamesTheBackup(t *testing.T) {
	w, sub, r := dumpWorld(t, 3) // backup, pre-restore dump, load
	before := len(w.fake.Calls())
	err := w.restore(sub, r, nil)
	if err == nil {
		t.Fatal("a dead load restored")
	}
	runs, rerr := w.f.Backups.Runs(ctx, w.vol.ID)
	must(t, rerr)
	var pre string
	for _, x := range runs {
		if x.Trigger == bk.PreRestore {
			pre = x.ID
		}
	}
	if pre == "" || !strings.Contains(err.Error(), pre) {
		t.Errorf("error %q does not name the pre-restore run %q", err, pre)
	}
	started := false
	for _, c := range w.fake.Calls()[before:] {
		started = started || c.Method == "Start" && c.Args[0] == "r1"
	}
	if !started {
		t.Error("the consumers were left stopped")
	}
}

// The cluster check runs before the pre-restore backup and before any stop;
// an old admin-only dump (no marker) skips it.
func TestClusterRestoreIsVettedFirst(t *testing.T) {
	w, sub, r := dumpWorld(t, 0)
	before := len(w.fake.Calls())
	err := w.restore(sub, r, func(context.Context) error { return errs.Refusedf("not here") })
	if err == nil || !strings.Contains(err.Error(), "not here") {
		t.Fatalf("err = %v", err)
	}
	if n := len(w.fake.Calls()) - before; n != 0 {
		t.Errorf("%d docker calls before the refusal: %v", n, w.fake.Calls()[before:])
	}

	w.fake.ExecOut = "SELECT 1;\n"
	old, err := w.f.Backup(ctx, sub, w.spec(0), io.Discard)
	must(t, err)
	called := false
	must(t, w.restore(sub, old, func(context.Context) error { called = true; return nil }))
	if called {
		t.Error("an admin-only dump was vetted as a cluster dump")
	}
}
