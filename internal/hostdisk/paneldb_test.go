package hostdisk_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/hostdisk/hostdisktest"
	_ "modernc.org/sqlite"
)

func req(h *hostdisktest.Host, op, mounted string, from, to hostdisk.DBFile) hostdisk.Request {
	return hostdisk.Request{Op: op, Kind: "paneldb", DB: &hostdisk.DBReq{
		DataDir: h.DataDir, DiskID: h.DiskID, InstallID: "inst1", Mounted: mounted, From: from, To: to}}
}

func f(area, name string) hostdisk.DBFile { return hostdisk.DBFile{Area: area, Name: name} }

func mustOK(t *testing.T, res hostdisk.Result) hostdisk.Result {
	t.Helper()
	if res.Error != nil {
		t.Fatalf("error: %v", res.Error)
	}
	return res
}

func wantCode(t *testing.T, res hostdisk.Result, code string) {
	t.Helper()
	if res.Error == nil || res.Error.Code != code {
		t.Fatalf("error = %v, want code %s", res.Error, code)
	}
}

// a database whose newest row is only in the -wal
func walOnlyDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t(v TEXT); INSERT INTO t VALUES ('old')"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCheckpointPutsWalRowsInTheMainFileAndCopyVerifies(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	db := walOnlyDB(t, h.DB())
	if _, err := db.Exec("INSERT INTO t VALUES ('wal-only')"); err != nil {
		t.Fatal(err)
	}
	// the connection stays open: the row lives in the -wal only
	mustOK(t, h.Run(ctx, req(h, "checkpoint", "no", f("db", "stackr.db"), hostdisk.DBFile{})))
	if fi, err := os.Stat(h.DB() + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("-wal holds %d bytes after the checkpoint", fi.Size())
	}
	cp := mustOK(t, h.Run(ctx, req(h, "copy", "no", f("db", "stackr.db"), f("folder", "stackr.db"))))
	hs := mustOK(t, h.Run(ctx, req(h, "hash", "no", f("db", "stackr.db"), hostdisk.DBFile{})))
	if cp.DB.SHA256 == "" || cp.DB.SHA256 != hs.DB.SHA256 {
		t.Errorf("copy %s source %s", cp.DB.SHA256, hs.DB.SHA256)
	}
	mustOK(t, h.Run(ctx, req(h, "integrity", "no", f("folder", "stackr.db"), hostdisk.DBFile{})))
	_ = db.Close()
	// the copy alone holds the row
	only, err := sql.Open("sqlite", "file:"+h.Folder()+"/stackr.db?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = only.Close() }()
	var n int
	if err := only.QueryRow("SELECT count(*) FROM t WHERE v='wal-only'").Scan(&n); err != nil || n != 1 {
		t.Errorf("copy has the wal-only row: n=%d err=%v", n, err)
	}
}

func TestEveryDBOpChecksTheMountState(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	h.WriteFile("/var/lib/stackr/db/stackr.db", []byte("x"))
	wantCode(t, h.Run(ctx, req(h, "hash", "yes", f("db", "stackr.db"), hostdisk.DBFile{})), hostdisk.CodeMountState)
	wantCode(t, h.Run(ctx, req(h, "hash", "", f("db", "stackr.db"), hostdisk.DBFile{})), hostdisk.CodeMountState)
	mustOK(t, h.Run(ctx, req(h, "hash", "no", f("db", "stackr.db"), hostdisk.DBFile{})))
	// swap and folder areas do not depend on it
	mustOK(t, h.Run(ctx, req(h, "list", "", f("swap", ""), hostdisk.DBFile{})))
}

func TestPlaceMarkerMountUnmountCycle(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	h.WriteFile("/var/lib/stackr/db/stackr.db", []byte("data"))
	mustOK(t, h.Run(ctx, req(h, "place", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	// no marker yet: refused, and nothing was locked
	wantCode(t, h.Run(ctx, req(h, "mount", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeBadMarker)
	mustOK(t, h.Run(ctx, req(h, "marker-write", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	// db/ still holds the database: lock refuses, so does mount
	wantCode(t, h.Run(ctx, req(h, "mount", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeNotEmpty)
	mustOK(t, h.Run(ctx, req(h, "rename", "no", f("db", "stackr.db"), f("swap", "stackr.db.moving"))))
	res := mustOK(t, h.Run(ctx, req(h, "mount", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	if !res.DB.Mounted || res.DB.Marker == nil || res.DB.Marker.InstallID != "inst1" {
		t.Errorf("mount result %+v", res.DB)
	}
	if !h.Mounted() || !h.Locked() {
		t.Errorf("mounted=%v locked=%v", h.Mounted(), h.Locked())
	}
	unit, err := os.ReadFile(h.Path("/etc/systemd/system/" + hostdisk.BindUnitName(h.DataDir)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# managed by stackr", "Before=docker.service", "RequiresMountsFor=/var/lib/stackr-disks/d1 /var/lib/stackr/db",
		"What=/var/lib/stackr-disks/d1/d1/stackr-panel-db", "Where=/var/lib/stackr/db", "Options=bind,nofail", "WantedBy=local-fs.target"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	// a second mount is a check, not an error
	mustOK(t, h.Run(ctx, req(h, "mount", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	// a file op on db/ expecting an unmounted db/ now refuses
	wantCode(t, h.Run(ctx, req(h, "clear-underlay", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeMountState)
	mustOK(t, h.Run(ctx, req(h, "unmount", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	mustOK(t, h.Run(ctx, req(h, "unmount", "", hostdisk.DBFile{}, hostdisk.DBFile{}))) // repeat is success
	if h.Mounted() {
		t.Error("still mounted")
	}
	if _, err := os.Stat(h.Path("/etc/systemd/system/" + hostdisk.BindUnitName(h.DataDir))); err == nil {
		t.Error("unit left")
	}
}

func TestForeignBindUnitIsNeverTouched(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	h.WriteFile("/etc/systemd/system/"+hostdisk.BindUnitName(h.DataDir), []byte("[Mount]\nWhat=/elsewhere\n"))
	mustOK(t, h.Run(ctx, req(h, "place", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	mustOK(t, h.Run(ctx, req(h, "marker-write", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	wantCode(t, h.Run(ctx, req(h, "mount", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeBusy)
	wantCode(t, h.Run(ctx, req(h, "unmount", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeBusy)
}

func TestRenameNeverReplacesAndRepeatsAreSuccess(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	h.WriteFile("/var/lib/stackr/db/stackr.db", []byte("a"))
	h.WriteFile("/var/lib/stackr/swap/stackr.db.moving", []byte("b"))
	wantCode(t, h.Run(ctx, req(h, "rename", "no", f("db", "stackr.db"), f("swap", "stackr.db.moving"))), hostdisk.CodeBusy)
	mustOK(t, h.Run(ctx, req(h, "rm", "no", f("swap", "stackr.db.moving"), hostdisk.DBFile{})))
	mustOK(t, h.Run(ctx, req(h, "rename", "no", f("db", "stackr.db"), f("swap", "stackr.db.moving"))))
	mustOK(t, h.Run(ctx, req(h, "rename", "no", f("db", "stackr.db"), f("swap", "stackr.db.moving")))) // already done
	r := req(h, "rename", "no", f("db", "stackr.db-wal"), f("swap", "stackr.db.moving-wal"))
	r.DB.Optional = true
	mustOK(t, h.Run(ctx, r))
	wantCode(t, h.Run(ctx, req(h, "rename", "no", f("db", "../x"), f("swap", "y"))), hostdisk.CodeBadName)
}

func TestLockNeedsAnEmptyUnmountedDBDir(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	h.WriteFile("/var/lib/stackr/db/stackr.db-shm", []byte("shm"))
	wantCode(t, h.Run(ctx, req(h, "lock", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeNotEmpty)
	mustOK(t, h.Run(ctx, req(h, "clear-underlay", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	mustOK(t, h.Run(ctx, req(h, "lock", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	if !h.Locked() {
		t.Error("not locked")
	}
	// a locked underlay gives EPERM to a database writer
	if err := os.WriteFile(h.DB(), nil, 0o600); err == nil {
		t.Error("a database could be created in the locked underlay")
	}
	mustOK(t, h.Run(ctx, req(h, "unlock", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
}

func TestProbeSymlinkedDataDirAndNoChattr(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	res := mustOK(t, h.Run(ctx, req(h, "probe", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	if !res.DB.ChattrOK || res.DB.SysFree == 0 {
		t.Errorf("probe %+v", res.DB)
	}
	h.FailOn = func(call string) error {
		if strings.HasPrefix(call, "chattr +i") {
			return os.ErrPermission
		}
		return nil
	}
	res = mustOK(t, h.Run(ctx, req(h, "probe", "", hostdisk.DBFile{}, hostdisk.DBFile{})))
	if res.DB.ChattrOK {
		t.Error("chattr reported working")
	}
	// a symlink on the way to the data dir is refused
	h2 := hostdisktest.New(t, "d1")
	if err := os.Rename(h2.Path("/var/lib/stackr"), h2.Path("/var/lib/real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/real", h2.Path("/var/lib/stackr")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, h2.Run(ctx, req(h2, "probe", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeSymlink)
}

func TestDiskMissingRefusesFolderOps(t *testing.T) {
	ctx := context.Background()
	h := hostdisktest.New(t, "d1")
	h.DiskGone()
	wantCode(t, h.Run(ctx, req(h, "place", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeDiskMissing)
	wantCode(t, h.Run(ctx, req(h, "mount", "", hostdisk.DBFile{}, hostdisk.DBFile{})), hostdisk.CodeDiskMissing)
}
