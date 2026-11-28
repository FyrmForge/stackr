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

func stubAnswers(t *testing.T, up bool) {
	t.Helper()
	old := panelAnswers
	t.Cleanup(func() { panelAnswers = old })
	panelAnswers = func(context.Context, installspec.Input, string) bool { return up }
}

// The new panel ran and its pre-upgrade copy is gone: the installer starts the
// recorded new panel (its image, its name) on the migrated database, removes the
// recorded predecessor, and clears the journal only after it answers.
func TestInstallerStartsTheNewPanelWhenTheCopyIsGone(t *testing.T) {
	dir := t.TempDir()
	if err := paneldb.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	put(t, paneldb.Path(dir), "migrated")
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: paneldb.DirUpdate,
		Dst: paneldb.Path(dir), Copy: filepath.Join(dir, "swap", "gone"), Container: "stackr-0.2.0", Ran: true,
		Image: "ghcr.io/fyrmforge/stackr:0.2.0", Predecessors: []string{"p1"}}); err != nil {
		t.Fatal(err)
	}
	log := fakeDockerBin(t, "case \"$1\" in inspect) exit 1;; esac\nexit 0\n")

	stubAnswers(t, false)
	in := installspec.Input{Root: "x.com", PanelHost: "p.x.com", HTTPPort: "80", DataDir: dir, Bind: "172.17.0.1"}
	if err := (runner{out: &bytes.Buffer{}}).panelDB(context.Background(), in); err == nil || !strings.Contains(err.Error(), "did not come up") {
		t.Fatalf("a panel that does not answer: %v", err)
	}
	if j, _ := paneldb.ReadJournal(dir); j.Phase == paneldb.PhaseNone {
		t.Fatal("the journal was cleared for a panel that is not healthy")
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "rm -f p1") {
		t.Fatal("the predecessor was removed before the new panel answered")
	}
	if err := paneldb.Check(dir, paneldb.Path(dir)); err != nil {
		t.Errorf("the new panel could not boot under the journal: %v", err)
	}

	stubAnswers(t, true)
	if err := (runner{out: &bytes.Buffer{}}).panelDB(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "run -d --name stackr-0.2.0") || !strings.Contains(string(b), "ghcr.io/fyrmforge/stackr:0.2.0") {
		t.Errorf("the recorded new panel was not started:\n%s", b)
	}
	if !strings.Contains(string(b), "rm -f p1") {
		t.Errorf("the predecessor was not removed:\n%s", b)
	}
	if j, _ := paneldb.ReadJournal(dir); j.Phase != paneldb.PhaseNone {
		t.Errorf("journal = %q", j.Phase)
	}
	if get(paneldb.Path(dir)) != "migrated" {
		t.Error("the migrated database was touched")
	}
}

// A rollback that can run starts only the recorded predecessor.
func TestInstallerRollbackStartsOnlyTheRecordedPredecessor(t *testing.T) {
	dir := t.TempDir()
	if err := paneldb.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	put(t, paneldb.Path(dir), "migrated")
	cp := filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")
	put(t, cp, "pre-upgrade")
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: paneldb.DirUpdate,
		Dst: paneldb.Path(dir), Copy: cp, Container: "stackr-0.2.0", Ran: true, Predecessors: []string{"p1"}}); err != nil {
		t.Fatal(err)
	}
	log := fakeDockerBin(t, "case \"$1\" in ps) printf 'p1\\np2\\n';; esac\nexit 0\n")
	stubAnswers(t, false)
	if err := (runner{out: &bytes.Buffer{}}).panelDB(context.Background(), installspec.Input{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "start p1") || strings.Contains(string(b), "start p2") {
		t.Errorf("docker calls:\n%s", b)
	}
	if get(paneldb.Path(dir)) != "pre-upgrade" {
		t.Errorf("db = %q", get(paneldb.Path(dir)))
	}
}

// A restore supersedes a swap journal whose copy is gone.
func TestRestoreSupersedesAnUnfinishedSwap(t *testing.T) {
	dir := t.TempDir()
	if err := paneldb.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	put(t, paneldb.Path(dir), "migrated")
	if err := paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: paneldb.DirUpdate,
		Dst: paneldb.Path(dir), Copy: filepath.Join(dir, "swap", "gone"), Ran: true}); err != nil {
		t.Fatal(err)
	}
	if err := restorePanelDB(dir); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if err := beginRestore(dir, "S"); err != nil {
		t.Fatal(err)
	}
	if err := installRestoredDB(dir, stageWith(t, "restored")); err != nil {
		t.Fatal(err)
	}
	if err := finishRestore(dir); err != nil {
		t.Fatal(err)
	}
	if j, _ := paneldb.ReadJournal(dir); j.Phase != paneldb.PhaseNone || get(paneldb.Path(dir)) != "restored" {
		t.Errorf("journal %q, db %q", j.Phase, get(paneldb.Path(dir)))
	}
	if err := paneldb.Check(dir, paneldb.Path(dir)); err != nil {
		t.Error(err)
	}
}
