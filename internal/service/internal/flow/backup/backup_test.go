package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

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

func (vipStub) Set(context.Context, string, []string) error {
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
