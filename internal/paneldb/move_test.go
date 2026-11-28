package paneldb

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// devStat answers os.Stat, but with another device for db/ and (optionally)
// another owner, which is what a mount of the disk folder looks like.
func devStat(dataDir string, otherDev bool, uid uint32) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		fi, err := os.Stat(p)
		if err != nil || filepath.Clean(p) != filepath.Join(dataDir, DirName) {
			return fi, err
		}
		st := *fi.Sys().(*syscall.Stat_t)
		if otherDev {
			st.Dev++
		}
		st.Uid = uid
		return statInfo{fi, &st}, nil
	}
}

type statInfo struct {
	fs.FileInfo
	st *syscall.Stat_t
}

func (s statInfo) Sys() any { return s.st }

func TestMovedBootCheckCases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		otherDev bool
		mode     os.FileMode
		uid      uint32
		marker   string
		want     string
	}{
		{"mounted, marker matches", true, 0o700, 0, `{"install_id":"i1","disk_id":"d1"}`, ""},
		{"disk missing: db/ is the underlay", false, 0o700, 0, "", "disk missing"},
		{"mounted, no marker", true, 0o700, 0, "", "no panel database marker"},
		{"mounted, another install", true, 0o700, 0, `{"install_id":"other","disk_id":"d1"}`, "another install"},
		{"mounted, another disk id", true, 0o700, 0, `{"install_id":"i1","disk_id":"d2"}`, "another install"},
		{"mounted, wrong mode", true, 0o755, 0, `{"install_id":"i1","disk_id":"d1"}`, "not 0700 root"},
		{"mounted, not root", true, 0o700, 1000, `{"install_id":"i1","disk_id":"d1"}`, "not 0700 root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			must(t, EnsureLayout(dir))
			must(t, os.Chmod(filepath.Join(dir, DirName), tc.mode))
			if tc.marker != "" {
				write(t, filepath.Join(dir, DirName, MarkerName), tc.marker)
			}
			write(t, Path(dir), "db")
			must(t, WriteJournal(dir, Journal{Phase: PhaseMoved, Direction: DirIn, DiskID: "d1", InstallID: "i1"}))
			old := stat
			stat = devStat(dir, tc.otherDev, tc.uid)
			defer func() { stat = old }()
			err := Check(dir, Path(dir))
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if tc.want != "" {
				if _, err := os.Stat(filepath.Join(dir, "keys")); err == nil {
					t.Error("a refusal made a key dir")
				}
			}
		})
	}
}

// A move-back's new panel runs on the system disk: nothing is mounted and the
// check must let it boot. A move-in's new panel needs the mount.
func TestStartingNewByDirection(t *testing.T) {
	dir := t.TempDir()
	must(t, EnsureLayout(dir))
	write(t, Path(dir), "db")
	must(t, WriteJournal(dir, Journal{Phase: PhaseGating, Direction: DirBack, DiskID: "d1", InstallID: "i1"}))
	if err := Check(dir, Path(dir)); err != nil {
		t.Errorf("move-back gate refused: %v", err)
	}
	must(t, WriteJournal(dir, Journal{Phase: PhaseGating, Direction: DirIn, DiskID: "d1", InstallID: "i1"}))
	if err := Check(dir, Path(dir)); err == nil || !strings.Contains(err.Error(), "disk missing") {
		t.Errorf("move-in gate on an unmounted db/: %v", err)
	}
	// an ordinary Update on a moved install keeps the mount rule
	must(t, WriteJournal(dir, Journal{Phase: PhaseGating, Direction: DirUpdate, Moved: true, DiskID: "d1", InstallID: "i1"}))
	if err := Check(dir, Path(dir)); err == nil || !strings.Contains(err.Error(), "disk missing") {
		t.Errorf("moved update gate on an unmounted db/: %v", err)
	}
}

func TestFinishKeepsTheMovedState(t *testing.T) {
	dir := t.TempDir()
	u := Journal{Phase: PhaseStartingNew, Direction: DirUpdate, Moved: true, DiskID: "d1", InstallID: "i1", Folder: "/f", MoveID: "m", OldCopy: "c", Copy: "x"}
	must(t, Finish(dir, u))
	j, _ := ReadJournal(dir)
	if j.Phase != PhaseMoved || j.DiskID != "d1" || j.InstallID != "i1" || j.Folder != "/f" || j.Moved || j.Copy != "" {
		t.Errorf("journal after a moved update = %+v", j)
	}
	must(t, Finish(dir, Journal{Phase: PhaseStartingNew, Direction: DirUpdate}))
	if j, _ := ReadJournal(dir); j.Phase != PhaseNone {
		t.Errorf("unmoved: %+v", j)
	}
}

func TestInstallerLetsAMoveThrough(t *testing.T) {
	dir := t.TempDir()
	must(t, EnsureLayout(dir))
	for _, d := range []string{DirIn, DirBack} {
		must(t, WriteJournal(dir, Journal{Phase: PhaseMounting, Direction: d}))
		if err := CheckInstaller(dir); err != nil {
			t.Errorf("%s: installer refused: %v", d, err)
		}
		if err := Check(dir, Path(dir)); err == nil {
			t.Errorf("%s: the panel boot let a move in progress through", d)
		}
	}
}

func TestCountable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{syscall.EIO, true},
		{&fs.PathError{Op: "write", Path: "x", Err: syscall.EROFS}, true},
		{&fs.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}, false},
		{syscall.EDQUOT, false},
		{errors.New("SQLITE_FULL: database or disk is full"), false},
		{errors.New("SQLITE_BUSY"), false},
		{errors.New("database is locked"), false},
	} {
		if got := Countable(tc.err); got != tc.want {
			t.Errorf("Countable(%v) = %v", tc.err, got)
		}
	}
}

func TestWatchExitsAfterConsecutiveFailures(t *testing.T) {
	run := func(seq []error) (exited bool, calls int) {
		var n atomic.Int32
		done := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go Watch(ctx, time.Millisecond, 3, 9, func() error {
			i := int(n.Add(1)) - 1
			if i < len(seq) {
				return seq[i]
			}
			return nil
		}, func(error) { done <- struct{}{} })
		select {
		case <-done:
			return true, int(n.Load())
		case <-time.After(200 * time.Millisecond):
			return false, int(n.Load())
		}
	}
	eio := syscall.EIO
	if ex, n := run([]error{eio, eio, eio}); !ex || n != 3 {
		t.Errorf("3 failures: exited=%v calls=%d", ex, n)
	}
	if ex, _ := run([]error{eio, eio, nil, eio, eio, nil}); ex {
		t.Error("a success between failures did not reset the count")
	}
	full := []error{syscall.ENOSPC, errors.New("SQLITE_FULL"), syscall.EDQUOT, errors.New("SQLITE_BUSY"), syscall.ENOSPC, syscall.ENOSPC}
	if ex, _ := run(full); ex {
		t.Error("a full disk or a busy database counted as a failure")
	}
	// the real probe writes one file and fsyncs it
	dir := t.TempDir()
	must(t, ProbeWrite(dir))
	if _, err := os.Stat(filepath.Join(dir, ProbeName)); err != nil {
		t.Error(err)
	}
}

// A move-in interrupted at starting-new, gating or passed, with the disk gone:
// the panel boot refuses, but the installer gets through to the helper, whose
// rollback or finish needs no disk.
func TestInstallerSkipsTheMountCheckOfAMoveIn(t *testing.T) {
	dir := t.TempDir()
	must(t, EnsureLayout(dir))
	write(t, filepath.Join(dir, InstallFile), "{}") // an install, db/ is the empty underlay
	for _, ph := range []string{PhaseStartingNew, PhaseGating, PhasePassed} {
		must(t, WriteJournal(dir, Journal{Phase: ph, Direction: DirIn, DiskID: "d1", InstallID: "i1"}))
		if err := CheckInstaller(dir); err != nil {
			t.Errorf("%s: installer refused: %v", ph, err)
		}
		if err := Check(dir, Path(dir)); err == nil || !strings.Contains(err.Error(), "disk missing") {
			t.Errorf("%s: the booting panel was not refused: %v", ph, err)
		}
	}
}
