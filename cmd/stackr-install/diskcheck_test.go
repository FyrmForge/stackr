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
	dataDir = t.TempDir()
	bind = hostdisk.BindUnitName(dataDir)
	if err := os.WriteFile(filepath.Join(systemdDir, bind), []byte("# managed by stackr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := paneldb.WriteJournal(dataDir, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1"}); err != nil {
		t.Fatal(err)
	}
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

// A bind unit left on disk with no moved journal (after a give-up or a move
// back) is never started: it would mount the stale disk database.
func TestDiskCheckSkipsAStaleBindUnit(t *testing.T) {
	dataDir := t.TempDir()
	started := withUnits(t, hostdisk.BindUnitName(dataDir), hostdisk.UnitName(hostdisk.MountPoint("d1")))
	t.Setenv("STACKR_DATA_DIR", dataDir)
	if err := diskCheck(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(*started) != 1 || strings.Contains((*started)[0], "db.mount") {
		t.Errorf("started %v", *started)
	}
}

// uninstall --purge refuses whenever db/ is a mount point, journal or not.
func TestUninstallRefusesAMountedDB(t *testing.T) {
	r := newUninstallRig(t)
	if err := os.WriteFile(filepath.Join(r.install, installspec.File), []byte(`{"panel_host":"stkr.example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reads["docker ps -aq --filter label=stackr.role=panel"] = ""
	for _, f := range []string{"node.name", "mesh.json"} {
		_ = os.Remove(filepath.Join(r.state, f))
	}
	old := isMounted
	isMounted = func(string) bool { return true }
	defer func() { isMounted = old }()
	if err := r.env(true, "").run(context.Background(), true); err == nil || !strings.Contains(err.Error(), "node disk") {
		t.Errorf("err = %v", err)
	}
}

// A unit whose disk left sysfs: every mount of its device is lazily unmounted,
// last first, then the unit is restarted. A live device is left alone.
func TestDiskCheckClearsADeadMount(t *testing.T) {
	xfs := hostdisk.UnitName(hostdisk.MountPoint("d1"))
	where := hostdisk.MountPoint("d1")
	started := withUnits(t, xfs)
	if err := os.WriteFile(filepath.Join(systemdDir, xfs), []byte("[Mount]\nWhere="+where+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sys := t.TempDir()
	oldTab, oldSys, oldUm, oldRe := mountTable, sysBlock, abandonUnmount, restartUnit
	t.Cleanup(func() { mountTable, sysBlock, abandonUnmount, restartUnit = oldTab, oldSys, oldUm, oldRe })
	sysBlock = sys
	var order []string
	mounted := true
	mountTable = func() []hostdisk.Mount {
		if !mounted {
			return nil
		}
		return []hostdisk.Mount{{Dev: "8:16", Point: where}, {Dev: "0:50", Point: "/other"}, {Dev: "8:16", Point: "/var/lib/docker/vol"}}
	}
	abandonUnmount = func(_ context.Context, dir string) error { order = append(order, "umount "+dir); return nil }
	restartUnit = func(_ context.Context, u string) error {
		order = append(order, "restart")
		mounted = true
		return os.MkdirAll(filepath.Join(sys, "8:16"), 0o755)
	}
	t.Setenv("STACKR_DATA_DIR", t.TempDir())

	// device gone: unmounts in reverse, then restart
	if err := diskCheck(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "umount /var/lib/docker/vol,umount "+where+",restart" || len(*started) != 0 {
		t.Errorf("dead: %q, started %v", got, *started)
	}

	// device present: plain start, no unmount
	order = nil
	if err := os.MkdirAll(filepath.Join(sys, "8:16"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := diskCheck(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(order) != 0 || len(*started) != 1 {
		t.Errorf("live: %v, started %v", order, *started)
	}

	// a start that leaves nothing mounted fails and prints the hint
	mounted = false
	*started = nil
	var out bytes.Buffer
	if err := diskCheck(context.Background(), nil, &out); err == nil || !strings.Contains(out.String(), "still holds the old disk") {
		t.Errorf("nothing mounted: err %v, out %q", err, out.String())
	}
}

func diskCheckRig(t *testing.T, xfsWhere, bindWhere string) (sys string, started *[]string) {
	t.Helper()
	xfs := hostdisk.UnitName(hostdisk.MountPoint("d1"))
	dataDir := t.TempDir()
	bind := hostdisk.BindUnitName(dataDir)
	started = withUnits(t, xfs, bind)
	for u, w := range map[string]string{xfs: xfsWhere, bind: bindWhere} {
		if err := os.WriteFile(filepath.Join(systemdDir, u), []byte("[Mount]\nWhere="+w+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := paneldb.WriteJournal(dataDir, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STACKR_DATA_DIR", dataDir)
	sys = t.TempDir()
	oldTab, oldSys, oldUm, oldRe := mountTable, sysBlock, abandonUnmount, restartUnit
	t.Cleanup(func() { mountTable, sysBlock, abandonUnmount, restartUnit = oldTab, oldSys, oldUm, oldRe })
	sysBlock = sys
	return sys, started
}

// A plain start failure (disk not attached yet) prints no held-container hint;
// a dead mount that survives the start is not "started".
func TestDiskCheckHintAndSurvivingDeadMount(t *testing.T) {
	where := hostdisk.MountPoint("d1")
	_, _ = diskCheckRig(t, where, "/data/db")
	mountTable = func() []hostdisk.Mount { return nil }
	startUnit = func(context.Context, string) error { return errors.New("no such device") }
	var out bytes.Buffer
	if err := diskCheck(context.Background(), nil, &out); err == nil || strings.Contains(out.String(), "still holds") {
		t.Errorf("plain failure: err %v out %q", err, out.String())
	}
	// the mount is listed but its device is not in sysfs and clearing found nothing to do (unmount was a no-op)
	startUnit = func(context.Context, string) error { return nil }
	mountTable = func() []hostdisk.Mount {
		return []hostdisk.Mount{{Dev: "8:16", Point: where}, {Dev: "8:16", Point: "/data/db"}}
	}
	abandonUnmount = func(context.Context, string) error { return nil }
	restartUnit = func(context.Context, string) error { return nil }
	out.Reset()
	err := diskCheck(context.Background(), nil, &out)
	if err == nil || strings.Contains(out.String(), "started") || !strings.Contains(out.String(), "still holds") {
		t.Errorf("surviving dead mount: err %v out %q", err, out.String())
	}
}

// The bind unit after the xfs pass is restarted too: its point was unmounted
// with the disk's, so it no longer shows in the table.
func TestDiskCheckRestartsTheBindUnitAfterAClearedMount(t *testing.T) {
	where := hostdisk.MountPoint("d1")
	_, started := diskCheckRig(t, where, "/data/db")
	unmounted := map[string]bool{}
	mountTable = func() []hostdisk.Mount {
		var ms []hostdisk.Mount
		for _, p := range []string{where, "/data/db"} {
			if !unmounted[p] {
				ms = append(ms, hostdisk.Mount{Dev: "8:16", Point: p})
			}
		}
		return ms
	}
	abandonUnmount = func(_ context.Context, p string) error { unmounted[p] = true; return nil }
	var restarted []string
	restartUnit = func(_ context.Context, u string) error {
		restarted = append(restarted, u)
		// the unit remounts on a live device
		unmounted = map[string]bool{}
		if err := os.MkdirAll(filepath.Join(sysBlock, "8:16"), 0o755); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	if err := diskCheck(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(restarted) != 2 || len(*started) != 0 {
		t.Errorf("restarted %v started %v", restarted, *started)
	}
}

// With no sysfs here nothing is told gone and nothing is unmounted.
func TestClearDeadMountNeedsSysfs(t *testing.T) {
	_, _ = diskCheckRig(t, "/mnt/x", "")
	sysBlock = filepath.Join(sysBlock, "absent")
	mountTable = func() []hostdisk.Mount { return []hostdisk.Mount{{Dev: "8:16", Point: "/mnt/x"}} }
	abandonUnmount = func(context.Context, string) error { t.Error("unmounted without sysfs"); return nil }
	if got := clearDeadMount(context.Background(), "/mnt/x"); got != nil {
		t.Errorf("cleared %v", got)
	}
}
