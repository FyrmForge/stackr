package backup

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	seg1 = "000000010000000000000001"
	seg2 = "000000010000000000000002"
	seg3 = "000000010000000000000003"
)

// readObject decrypts the object at key with the destination's key.
func readObject(t *testing.T, w *world, key string) []byte {
	t.Helper()
	obj, err := open(w.dest).Get(ctx, key)
	must(t, err)
	defer func() { _ = obj.Close() }()
	plain, err := decrypt(obj, w.dest.ArchiveKey)
	must(t, err)
	b, err := io.ReadAll(plain)
	must(t, err)
	return b
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	must(t, err)
	out, err := io.ReadAll(gz)
	must(t, err)
	return string(out)
}

func (w *world) calls(method string) [][]string {
	var out [][]string
	for _, c := range w.fake.Calls() {
		if c.Method == method {
			out = append(out, c.Args)
		}
	}
	return out
}

// oldBase records a finished base run that finished daysAgo, started at wal,
// with its object on the destination.
func (w *world) oldBase(t *testing.T, root string, daysAgo int, wal string) string {
	t.Helper()
	r, err := w.f.Backups.StartIn(ctx, bk.KindVolume, &w.vol.ID, nil, w.dest.ID, "schedule", nil)
	must(t, err)
	key := root + "/base/" + time.Now().AddDate(0, 0, -daysAgo).UTC().Format("20060102-150405") + "-data.tar.gz.age"
	must(t, open(w.dest).Put(ctx, key, strings.NewReader("old")))
	r, err = w.f.Backups.SetWalStart(ctx, r, wal)
	must(t, err)
	r, err = w.f.Backups.Finish(ctx, r, key, 3, nil)
	must(t, err)
	then := time.Now().AddDate(0, 0, -daysAgo)
	r.FinishedAt = &then
	must(t, w.st.BackupRuns.Update(ctx, r))
	return key
}

// A base backup: the WAL position lands on the run row, the engine's gzip is
// stored as-is (not gzipped twice) under pitr/base, and the retention that
// follows drops the base outside the window and the WAL only it needed.
func TestBaseBackupRecordsWalStartAndPrunesTheWindow(t *testing.T) {
	w := setup(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	basegz := tarGz(t, "base_backup", "pgdata")
	w.fake.ExecFn = func(cmd []string) string {
		if slices.Contains(cmd, "walfile") {
			return seg3 + "\n"
		}
		return string(basegz)
	}
	// older than the 7 day window: the 20 day one is the anchor, the 30 day
	// one goes, and with it the WAL that ends before the anchor's start
	gone := w.oldBase(t, root, 30, seg1)
	anchor := w.oldBase(t, root, 20, seg2)
	walOld := bk.WALKey(root, seg1, seg1)
	walNeeded := bk.WALKey(root, seg2, seg3)
	hist := root + "/wal/00000002.history-00000002.history" + bk.BundleExt
	for _, k := range []string{walOld, walNeeded, hist} {
		must(t, open(w.dest).Put(ctx, k, strings.NewReader("w")))
	}

	sub := Subject{
		Volume: w.vol, Engine: w.api, Base: true,
		Dump: []string{"pg_basebackup"}, WalStart: []string{"psql", "walfile"},
	}
	r, err := w.f.Backup(ctx, sub, Spec{Dest: w.dest, Prefix: root + "/base", Trigger: "manual", PITRDays: 7}, io.Discard)
	must(t, err)
	if r.Status != bk.Done || r.WalStart == nil || *r.WalStart != seg3 {
		t.Fatalf("run = %+v", r)
	}
	if !strings.HasPrefix(r.ObjectKey, root+"/base/") || !strings.HasSuffix(r.ObjectKey, "-data.tar.gz.age") {
		t.Errorf("key = %s", r.ObjectKey)
	}
	if got := readObject(t, w, r.ObjectKey); !bytes.Equal(got, basegz) {
		t.Error("the base is not stored exactly as the engine gzipped it")
	}
	keys, _ := open(w.dest).List(ctx, root+"/")
	for _, k := range []string{gone, walOld} {
		if slices.Contains(keys, k) {
			t.Errorf("%s survived: %v", k, keys)
		}
	}
	for _, k := range []string{anchor, walNeeded, hist, r.ObjectKey} {
		if !slices.Contains(keys, k) {
			t.Errorf("%s was pruned: %v", k, keys)
		}
	}
	if rs, _ := w.f.Backups.Bases(ctx, root); len(rs) != 2 {
		t.Errorf("%d base rows left, want 2", len(rs))
	}
}

// A base whose WAL position cannot be read fails before anything is written.
func TestBaseBackupFailsWithoutAWalPosition(t *testing.T) {
	w := setup(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	w.fake.ExecFn = func([]string) string { return "garbage" }
	sub := Subject{Volume: w.vol, Engine: w.api, Base: true, Dump: []string{"pg_basebackup"}, WalStart: []string{"psql"}}
	r, err := w.f.Backup(ctx, sub, Spec{Dest: w.dest, Prefix: root + "/base", Trigger: "manual", PITRDays: 7}, io.Discard)
	if err == nil || r.Status != bk.Failed || r.ObjectKey != "" {
		t.Fatalf("err %v run %+v", err, r)
	}
}

func (w *world) ship() Ship {
	return Ship{Engine: w.api, Dest: w.dest, Root: bk.PITRRoot("o1", w.vol.ID), Dir: "/archive"}
}

// ship answers ls with files, tar with tarOut.
func (w *world) archive(files []string, tarOut string) {
	w.fake.ExecFn = func(cmd []string) string {
		switch cmd[0] {
		case "ls":
			return strings.Join(files, "\n") + "\n"
		case "tar":
			return tarOut
		case "du":
			return "2048\t/archive\n"
		}
		return ""
	}
}

// A tick bundles the segments, ships them encrypted, then removes exactly
// those files; stray names are never passed to tar or rm; a history file
// travels alone.
func TestShipWALBundlesAndRemovesWhatItShipped(t *testing.T) {
	w := setup(t)
	w.archive([]string{seg2, seg1, seg1 + ".00000028.backup", "00000002.history", ".tmp-x", "junk; rm -rf", seg3 + ".partial"}, "TARBYTES")
	res, err := w.f.ShipWAL(ctx, w.ship(), io.Discard)
	must(t, err)
	root := bk.PITRRoot("o1", w.vol.ID)
	wantSeg := bk.WALKey(root, seg1, seg2)
	wantHist := root + "/wal/00000002.history-00000002.history" + bk.BundleExt
	keys, _ := open(w.dest).List(ctx, root+"/wal/")
	if !slices.Equal(keys, []string{wantSeg, wantHist}) {
		t.Fatalf("keys = %v", keys)
	}
	if got := gunzip(t, readObject(t, w, wantSeg)); got != "TARBYTES" {
		t.Errorf("bundle = %q", got)
	}
	if res.Bundles != 2 || res.Last != seg2 || res.Waiting != 0 || res.DirBytes != 2048<<10 || res.Bytes <= 0 {
		t.Errorf("result = %+v", res)
	}
	var tars, rms []string
	for _, c := range w.calls("ExecStream") {
		switch c[1] {
		case "tar":
			tars = append(tars, strings.Join(c[2:], " "))
		case "rm":
			rms = append(rms, strings.Join(c[2:], " "))
		}
	}
	if !slices.Equal(tars, []string{"-C /archive -c " + seg1 + " " + seg1 + ".00000028.backup " + seg2, "-C /archive -c 00000002.history"}) {
		t.Errorf("tars = %v", tars)
	}
	if !slices.Equal(rms, []string{
		"-f /archive/" + seg1 + " /archive/" + seg1 + ".00000028.backup /archive/" + seg2,
		"-f /archive/00000002.history",
	}) {
		t.Errorf("rms = %v", rms)
	}
}

// Segments of two timelines in one tick ship as one bundle per timeline.
func TestShipWALNeverSpansTimelines(t *testing.T) {
	w := setup(t)
	t2a, t2b := "000000020000000000000002", "000000020000000000000003"
	w.archive([]string{t2b, seg1, t2a, seg2}, "TAR")
	res, err := w.f.ShipWAL(ctx, w.ship(), io.Discard)
	must(t, err)
	root := bk.PITRRoot("o1", w.vol.ID)
	keys, _ := open(w.dest).List(ctx, root+"/wal/")
	if want := []string{bk.WALKey(root, seg1, seg2), bk.WALKey(root, t2a, t2b)}; !slices.Equal(keys, want) || res.Bundles != 2 {
		t.Fatalf("keys = %v bundles %d", keys, res.Bundles)
	}
}

// Nothing is deleted before the Put succeeded.
func TestShipWALKeepsFilesWhenThePutFails(t *testing.T) {
	w := setup(t)
	blocker := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(blocker, nil, 0o600))
	w.dest.Endpoint = filepath.Join(blocker, "under-a-file") // MkdirAll fails
	w.archive([]string{seg1}, "TARBYTES")
	if _, err := w.f.ShipWAL(ctx, w.ship(), io.Discard); err == nil {
		t.Fatal("a failing Put shipped")
	}
	for _, c := range w.calls("ExecStream") {
		if c[1] == "rm" {
			t.Fatalf("rm ran after a failed Put: %v", c)
		}
	}
}

// A tar that dies halfway leaves no object and no rm.
func TestShipWALTarFailureShipsNothing(t *testing.T) {
	w := setup(t)
	w.archive([]string{seg1}, "TARBYTES")
	w.fake.Err = map[string]error{"ExecStream": errors.New("boom")}
	if _, err := w.f.ShipWAL(ctx, w.ship(), io.Discard); err == nil {
		t.Fatal("ls failure shipped")
	}
	keys, _ := open(w.dest).List(ctx, bk.PITRRoot("o1", w.vol.ID)+"/")
	if len(keys) != 0 {
		t.Errorf("keys = %v", keys)
	}
}

func TestShipWALIdleTick(t *testing.T) {
	w := setup(t)
	w.archive(nil, "")
	res, err := w.f.ShipWAL(ctx, w.ship(), io.Discard)
	must(t, err)
	if res.Bundles != 0 || res.Waiting != 0 || res.Last != "" {
		t.Errorf("result = %+v", res)
	}
}

// The guard: a non-empty prefix with no run of ours is refused; the
// instance's own history, or an empty prefix, passes.
func TestGuard(t *testing.T) {
	w := setup(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	must(t, w.f.Guard(ctx, w.dest, root))
	must(t, open(w.dest).Put(ctx, bk.WALKey(root, seg1, seg1), strings.NewReader("x")))
	err := w.f.Guard(ctx, w.dest, root)
	if !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("a foreign archive passed: %v", err)
	}
	r, err := w.f.Backups.StartIn(ctx, bk.KindVolume, &w.vol.ID, nil, w.dest.ID, "manual", nil)
	must(t, err)
	_, err = w.f.Backups.Finish(ctx, r, root+"/base/20261107-000000-data.tar.gz.age", 1, nil)
	must(t, err)
	must(t, w.f.Guard(ctx, w.dest, root))
	// another volume's root is not covered by that run
	if err := w.f.Guard(ctx, w.dest, bk.PITRRoot("o1", "other")); err != nil {
		t.Errorf("an empty root refused: %v", err)
	}
}

var _ = store.BackupRun{}
