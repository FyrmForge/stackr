package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

func withUnits(t *testing.T, names ...string) (started *[]string) {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldStart := systemdDir, startUnit
	systemdDir = dir
	var got []string
	startUnit = func(_ context.Context, u string) error { got = append(got, u); return nil }
	t.Cleanup(func() { systemdDir, startUnit = oldDir, oldStart })
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("# managed by stackr\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &got
}

// disk-check starts the disks' xfs units, then the database's bind unit, and
// nothing else (a foreign unit, a service).
func TestDiskCheckStartsTheUnitsInOrder(t *testing.T) {
	dataDir := "/var/lib/stackr"
	xfs := hostdisk.UnitName(hostdisk.MountPoint("d1"))
	bind := hostdisk.BindUnitName(dataDir)
	started := withUnits(t, bind, xfs, "docker.service", "mnt-other.mount")
	t.Setenv("STACKR_DATA_DIR", dataDir)
	var out bytes.Buffer
	if err := diskCheck(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(*started) != 2 || (*started)[0] != xfs || (*started)[1] != bind {
		t.Errorf("started %v, want the xfs unit then %s", *started, bind)
	}
	if !strings.Contains(out.String(), "restart loop") {
		t.Errorf("output %q", out.String())
	}
}

func TestDiskCheckReportsEveryFailureAndNoUnitsIsFine(t *testing.T) {
	started := withUnits(t)
	var out bytes.Buffer
	if err := diskCheck(context.Background(), nil, &out); err != nil || len(*started) != 0 {
		t.Fatalf("no units: err %v started %v", err, *started)
	}
	xfs := hostdisk.UnitName(hostdisk.MountPoint("d1"))
	withUnits(t, xfs)
	startUnit = func(context.Context, string) error { return errors.New("no such device") }
	if err := diskCheck(context.Background(), nil, &out); err == nil || !strings.Contains(err.Error(), "no such device") {
		t.Errorf("err = %v", err)
	}
	if err := diskCheck(context.Background(), []string{"x"}, &out); err == nil {
		t.Error("an argument was accepted")
	}
}

// A rerun with a move unfinished hands it to the helper (dry run says so),
// and a journal at moved with its disk missing is refused before anything runs.
func TestRerunResumesAMoveAndRefusesAMissingDisk(t *testing.T) {
	dir := t.TempDir()
	if err := paneldb.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paneldb.Path(dir), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := runner{dry: true, out: &out}
	in := installspec.Input{DataDir: dir}
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseMounting, Direction: paneldb.DirIn, MoveID: "m", Image: installspec.Image("0.8.40")}); err != nil {
		t.Fatal(err)
	}
	if err := r.panelDB(context.Background(), in); err != nil || !strings.Contains(out.String(), "unfinished database move") {
		t.Errorf("err %v out %q", err, out.String())
	}
	withUnits(t) // no systemd units
	live := runner{out: &out}
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1", InstallID: "i"}); err != nil {
		t.Fatal(err)
	}
	if err := live.panelDB(context.Background(), in); err == nil || !strings.Contains(err.Error(), "disk missing") {
		t.Errorf("moved with the disk missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys")); err == nil {
		t.Error("a refusal made keys")
	}
}

// uninstall --purge would rm -rf into the mounted database folder.
func TestUninstallRefusesWhileTheDatabaseIsOnADisk(t *testing.T) {
	r := newUninstallRig(t)
	if err := os.WriteFile(filepath.Join(r.install, installspec.File), []byte(`{"panel_host":"stkr.example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := paneldb.WriteJournal(r.install, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1"}); err != nil {
		t.Fatal(err)
	}
	r.reads["docker ps -aq --filter label=stackr.role=panel"] = ""
	for _, f := range []string{"node.name", "mesh.json"} { // a controller has no worker state
		_ = os.Remove(filepath.Join(r.state, f))
	}
	err := r.env(true, "").run(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "node disk") {
		t.Errorf("err = %v", err)
	}
}

// A restore is refused on a moved install, also when an Update on it was
// interrupted (its journal carries the moved state): the restore would rename
// across filesystems.
func TestRestoreRefusedOnAMovedInstall(t *testing.T) {
	for _, j := range []paneldb.Journal{
		{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1"},
		{Phase: paneldb.PhaseGating, Direction: paneldb.DirUpdate, Moved: true, DiskID: "d1"},
	} {
		dir := t.TempDir()
		if err := paneldb.WriteJournal(dir, j); err != nil {
			t.Fatal(err)
		}
		if err := restorePanelDB(dir); err == nil {
			t.Errorf("restore let %+v through", j)
		}
	}
}
