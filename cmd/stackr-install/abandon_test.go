package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

type abandonRig struct {
	dir      string
	mounted  bool
	healthy  bool // the mounted disk answers a write
	locked   bool
	calls    []string
	unitFile string
}

func newAbandonRig(t *testing.T) *abandonRig {
	t.Helper()
	r := &abandonRig{dir: t.TempDir(), mounted: true, locked: true}
	units := t.TempDir()
	oldDir := systemdDir
	systemdDir = units
	r.unitFile = filepath.Join(units, hostdisk.BindUnitName(r.dir))
	if err := os.WriteFile(r.unitFile, []byte("# managed by stackr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := paneldb.EnsureLayout(r.dir); err != nil {
		t.Fatal(err)
	}
	if err := paneldb.WriteJournal(r.dir, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1", InstallID: "i1"}); err != nil {
		t.Fatal(err)
	}
	olds := []any{abandonSystemctl, abandonUnmount, abandonUnlock, abandonCrash, isMounted, probeWrite}
	abandonSystemctl = func(_ context.Context, a ...string) error {
		r.calls = append(r.calls, "systemctl "+strings.Join(a, " "))
		if a[0] == "stop" {
			r.mounted = false
		}
		return nil
	}
	abandonUnmount = func(context.Context, string) error { r.calls = append(r.calls, "umount"); r.mounted = false; return nil }
	abandonUnlock = func(context.Context, string) error { r.calls = append(r.calls, "chattr -i"); r.locked = false; return nil }
	isMounted = func(string) bool { return r.mounted }
	probeWrite = func(string) error {
		if r.healthy {
			return nil
		}
		return errors.New("input/output error")
	}
	t.Cleanup(func() {
		systemdDir = oldDir
		abandonSystemctl, abandonUnmount, abandonUnlock = olds[0].(func(context.Context, ...string) error), olds[1].(func(context.Context, string) error), olds[2].(func(context.Context, string) error)
		abandonCrash, isMounted, probeWrite = olds[3].(func(string) error), olds[4].(func(string) bool), olds[5].(func(string) error)
	})
	return r
}

func TestAbandonDiskHappyPath(t *testing.T) {
	r := newAbandonRig(t)
	var out bytes.Buffer
	if err := abandonDisk(context.Background(), r.dir, &out); err != nil {
		t.Fatal(err)
	}
	j, _ := paneldb.ReadJournal(r.dir)
	if j.Phase != paneldb.PhaseNone || r.mounted || r.locked {
		t.Fatalf("journal %+v mounted=%v locked=%v", j, r.mounted, r.locked)
	}
	got := strings.Join(r.calls, "; ")
	if !strings.Contains(got, "systemctl disable") || strings.Contains(got, "xfs") {
		t.Errorf("calls %s", got)
	}
	if _, err := os.Stat(r.unitFile); err != nil {
		t.Error("the unit file went; it is only disabled")
	}
	// a plain restore of a moved install names the flag
	if err := paneldb.WriteJournal(r.dir, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1"}); err != nil {
		t.Fatal(err)
	}
	if err := restorePanelDB(r.dir); err == nil || !strings.Contains(err.Error(), "--abandon-disk") {
		t.Errorf("plain restore: %v", err)
	}
}

// A mounted disk that answers writes is healthy: move back, do not abandon.
func TestAbandonRefusesAHealthyDisk(t *testing.T) {
	r := newAbandonRig(t)
	r.healthy = true
	if err := abandonPrecheck(r.dir); err == nil || !strings.Contains(err.Error(), "move the database back") {
		t.Errorf("err = %v", err)
	}
	if err := restorePanelDBFor(r.dir, true); err == nil {
		t.Error("the restore flag let a healthy disk through")
	}
	// mounted but failing, and not mounted at all: allowed
	r.healthy = false
	if err := abandonPrecheck(r.dir); err != nil {
		t.Errorf("failing disk: %v", err)
	}
	r.mounted = false
	if err := abandonPrecheck(r.dir); err != nil {
		t.Errorf("missing disk: %v", err)
	}
}

// A crash at each step: the journal says abandoning (or none, once done), a
// plain boot and installer refuse in between, and running it again finishes.
func TestAbandonCrashAtEachStep(t *testing.T) {
	for _, step := range []string{"journaled", "stopped", "disabled", "unlocked", "journal-cleared"} {
		t.Run(step, func(t *testing.T) {
			r := newAbandonRig(t)
			abandonCrash = func(s string) error {
				if s == step {
					return fmt.Errorf("crash at %s", s)
				}
				return nil
			}
			if err := abandonDisk(context.Background(), r.dir, &bytes.Buffer{}); err == nil {
				t.Fatal("no crash")
			}
			j, _ := paneldb.ReadJournal(r.dir)
			if step != "journal-cleared" {
				if j.Phase != paneldb.PhaseAbandoning || j.DiskID != "d1" {
					t.Fatalf("journal after the crash = %+v", j)
				}
				// the panel and the installer refuse while it is half done
				if err := paneldb.Check(r.dir, paneldb.Path(r.dir)); err == nil || !strings.Contains(err.Error(), "--abandon-disk") {
					t.Errorf("boot check: %v", err)
				}
				if err := paneldb.CheckInstaller(r.dir); err == nil {
					t.Error("the installer let a half abandoned disk through")
				}
				if err := restorePanelDB(r.dir); err == nil {
					t.Error("a plain restore let it through")
				}
				if err := restorePanelDBFor(r.dir, true); err != nil {
					t.Errorf("the flag does not resume it: %v", err)
				}
			}
			abandonCrash = func(string) error { return nil }
			if err := abandonDisk(context.Background(), r.dir, &bytes.Buffer{}); err != nil {
				t.Fatalf("rerun: %v", err)
			}
			j, _ = paneldb.ReadJournal(r.dir)
			if j.Phase != paneldb.PhaseNone || r.mounted || r.locked {
				t.Errorf("end: journal %+v mounted=%v locked=%v", j, r.mounted, r.locked)
			}
			// nothing left to abandon: a rerun is a normal restore
			if err := abandonDisk(context.Background(), r.dir, &bytes.Buffer{}); err != nil {
				t.Errorf("third run: %v", err)
			}
		})
	}
}

// An unmounted bind that cannot be stopped is lazily unmounted; one that stays
// mounted stops the abandon with the journal kept.
func TestAbandonStuckMount(t *testing.T) {
	r := newAbandonRig(t)
	abandonSystemctl = func(_ context.Context, a ...string) error { return errors.New("hung") }
	abandonUnmount = func(context.Context, string) error { return errors.New("busy") }
	if err := abandonDisk(context.Background(), r.dir, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "still mounted") {
		t.Fatalf("err = %v", err)
	}
	if j, _ := paneldb.ReadJournal(r.dir); j.Phase != paneldb.PhaseAbandoning {
		t.Errorf("journal %+v", j)
	}
}
