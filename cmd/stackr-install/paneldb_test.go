package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func get(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// A restore refuses while the journal says a layout change or a move is not
// finished, and while the database is on a disk.
func TestRestorePanelDBRefusals(t *testing.T) {
	for name, j := range map[string]paneldb.Journal{
		"copying": {Phase: "copied", Direction: "in"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := paneldb.WriteJournal(dir, j); err != nil {
				t.Fatal(err)
			}
			if err := restorePanelDB(dir); err == nil {
				t.Error("restore went ahead")
			}
		})
	}
	dir := t.TempDir()
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseMoved, DiskID: "d1"}); err != nil {
		t.Fatal(err)
	}
	if err := restorePanelDB(dir); err == nil || !strings.Contains(err.Error(), "disk") {
		t.Errorf("moved database: %v", err)
	}
	if err := restorePanelDB(t.TempDir()); err != nil {
		t.Errorf("a plain box: %v", err)
	}
}

// A fresh install makes db/ and the placeholder before any key; an old
// install's database file is left to the panel swap; a journal that says a
// move is in progress stops the installer before it touches anything.
func TestInstallerPanelDB(t *testing.T) {
	ctx := context.Background()
	fresh := t.TempDir()
	if err := (runner{out: &bytes.Buffer{}}).panelDB(ctx, installspec.Input{DataDir: fresh}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(paneldb.Legacy(fresh)); err != nil || !fi.IsDir() {
		t.Errorf("fresh install: no placeholder (%v)", err)
	}
	if fi, err := os.Stat(filepath.Join(fresh, "db")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("fresh install: db/ = %v %v", fi, err)
	}

	old := t.TempDir()
	put(t, paneldb.Legacy(old), "db")
	if err := (runner{out: &bytes.Buffer{}}).panelDB(ctx, installspec.Input{DataDir: old}); err != nil {
		t.Fatal(err)
	}
	if get(paneldb.Legacy(old)) != "db" {
		t.Error("the installer moved an old install's database itself")
	}

	moving := t.TempDir()
	if err := paneldb.WriteJournal(moving, paneldb.Journal{Phase: "copied", Direction: "in"}); err != nil {
		t.Fatal(err)
	}
	before := len(listDir(t, moving))
	if err := (runner{out: &bytes.Buffer{}}).panelDB(ctx, installspec.Input{DataDir: moving}); err == nil {
		t.Fatal("a move in progress was let through")
	}
	if len(listDir(t, moving)) != before {
		t.Error("the installer touched the data dir while refusing")
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error { out = append(out, p); return err })
	return out
}

// The helper died after the new panel's gate passed and before it cleared the
// journal. The installer rerun keeps that panel and its database: the stale
// copy is dropped and the journal cleared, nothing is restored.
func TestInstallerKeepsALivePanelWhoseJournalWasNotCleared(t *testing.T) {
	dir := t.TempDir()
	if err := paneldb.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	put(t, paneldb.Path(dir), "live data since the upgrade")
	cp := filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")
	put(t, cp, "stale copy")
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: paneldb.DirLayout,
		Src: paneldb.Legacy(dir), Dst: paneldb.Path(dir), Copy: cp, Container: "stackr-0.2.0"}); err != nil {
		t.Fatal(err)
	}
	old := panelAnswers
	t.Cleanup(func() { panelAnswers = old })
	var asked string
	panelAnswers = func(_ context.Context, _ installspec.Input, name string) bool { asked = name; return true }
	if err := (runner{out: &bytes.Buffer{}}).panelDB(context.Background(), installspec.Input{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if asked != "stackr-0.2.0" {
		t.Errorf("asked about %q", asked)
	}
	if get(paneldb.Path(dir)) != "live data since the upgrade" {
		t.Errorf("database = %q", get(paneldb.Path(dir)))
	}
	if j, _ := paneldb.ReadJournal(dir); j.Phase != paneldb.PhaseNone {
		t.Errorf("journal = %q", j.Phase)
	}
	if _, err := os.Stat(cp); err == nil {
		t.Error("the stale copy is still there")
	}
}

// An install whose database vanished is never turned into a fresh one by the
// installer, with the layout there or not.
func TestInstallerRefusesAnInstallWithoutItsDatabase(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, installspec.File), "{}")
	if err := (runner{out: &bytes.Buffer{}}).panelDB(context.Background(), installspec.Input{DataDir: dir}); err == nil {
		t.Fatal("the installer went ahead")
	}
	if _, err := os.Stat(filepath.Join(dir, "db")); err == nil {
		t.Error("the installer made db/ for an install with no database")
	}
}

// An unfinished layout change is undone by the installer (dry run says so and
// changes nothing; the real one needs Docker, which the recovery itself does not).
func TestInstallerDryRunLeavesLayoutJournal(t *testing.T) {
	dir := t.TempDir()
	put(t, paneldb.Path(dir), "db")
	if err := paneldb.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseLayout, Direction: paneldb.DirLayout, Src: paneldb.Legacy(dir), Dst: paneldb.Path(dir)}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (runner{dry: true, out: &out}).panelDB(context.Background(), installspec.Input{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would undo") {
		t.Errorf("out = %q", out.String())
	}
	if get(paneldb.Path(dir)) != "db" {
		t.Error("a dry run moved the database")
	}
}

// A resumed move's helper carries the move label, so the panel's boot settles
// it as a move helper and not as a failed Update.
func TestResumeMoveLabelsTheHelper(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$1\" in inspect) exit 1;; esac\nexit 0\n"
	put(t, filepath.Join(dir, "docker"), script)
	if err := os.Chmod(filepath.Join(dir, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	in := installspec.Input{Root: "x.io", PanelHost: "p.x.io", HTTPPort: "80", DataDir: dir, Bind: "172.17.0.1"}
	j := paneldb.Journal{Phase: "copied", Direction: "in", MoveID: "m42", Image: installspec.Repo + ":0.8.2", Container: "stackr-0.8.2"}
	if err := (runner{out: &bytes.Buffer{}}).resumeMove(context.Background(), in, j); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--label "+installspec.LabelMove+"=m42") {
		t.Fatalf("helper run lacks the move label:\n%s", b)
	}
}
