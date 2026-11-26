package panel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/paneldb"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

// hook runs code at the moments the swap hands over to a container.
type hook struct {
	*dockerfake.Fake
	onRun   func(spec docker.ContainerSpec)
	onStart func(id string)
	// firstInspect, when set, is what the first Inspect of a container sees: the
	// later ones see the fake's details.
	firstInspect map[string]docker.Detail
	// stuck: Stop returns without the container exiting.
	stuck bool
}

func (h *hook) Inspect(ctx context.Context, id string) (docker.Detail, error) {
	if d, ok := h.firstInspect[id]; ok {
		delete(h.firstInspect, id)
		return d, nil
	}
	return h.Fake.Inspect(ctx, id)
}

func (h *hook) Run(ctx context.Context, sp docker.ContainerSpec) (string, error) {
	if h.onRun != nil {
		h.onRun(sp)
	}
	return h.Fake.Run(ctx, sp)
}

func (h *hook) Stop(ctx context.Context, id string) error {
	if h.stuck {
		return nil
	}
	return h.Fake.Stop(ctx, id)
}

func (h *hook) Start(ctx context.Context, id string) error {
	if h.onStart != nil {
		h.onStart(id)
	}
	return h.Fake.Start(ctx, id)
}

// oldInstall is a data dir as the last release left it: a database file with
// its wal at <dir>/stackr.db.
func oldInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "stackr.db"), "old db")
	write(t, filepath.Join(dir, "stackr.db-wal"), "old wal")
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// oldSpec is what the old panel (built before the layout change) hands over:
// the data dir at the old database path.
func oldSpec(dir string) docker.ContainerSpec {
	return docker.ContainerSpec{
		Name: "stackr-0.2.0", Image: "img:0.2.0",
		Env: []string{"DATA_DIR=" + dir, "DATABASE_PATH=" + filepath.Join(dir, "stackr.db")},
	}
}

func newHook(health string) *hook {
	fake := dockerfake.New()
	fake.RunID = "new"
	fake.Containers = []docker.Container{{ID: "old", State: "running", Labels: map[string]string{LabelRole: "panel"}}}
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: health}}
	return &hook{Fake: fake}
}

func leaf(h *hook) *Leaf {
	l := New(h)
	l.Poll, l.Deadline = time.Millisecond, 20*time.Millisecond
	return l
}

func journalPhase(t *testing.T, dir string) string {
	t.Helper()
	j, err := paneldb.ReadJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return j.Phase
}

// The first Update after the layout change: the old panel's spec says the old
// path, the helper moves the files into db/, leaves the placeholder, hands the
// new panel the new path, and clears the journal on a pass.
func TestLayoutChangeOldSpecPasses(t *testing.T) {
	dir := oldInstall(t)
	h := newHook("healthy")
	var envAtRun []string
	var atRun struct{ main, wal, note string }
	var phaseAtRun string
	h.onRun = func(sp docker.ContainerSpec) {
		envAtRun = sp.Env
		atRun.main = read(paneldb.Path(dir))
		atRun.wal = read(paneldb.Path(dir) + "-wal")
		atRun.note = read(filepath.Join(dir, "stackr.db", paneldb.NoteName))
		phaseAtRun = journalPhase(t, dir)
	}
	if err := leaf(h).Swap(context.Background(), oldSpec(dir)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(envAtRun, " "), "DATABASE_PATH="+paneldb.Path(dir)) {
		t.Errorf("new panel env = %v", envAtRun)
	}
	if atRun.main != "old db" || atRun.wal != "old wal" || atRun.note != paneldb.Note {
		t.Errorf("at run: %+v", atRun)
	}
	if phaseAtRun != paneldb.PhaseStartingNew {
		t.Errorf("journal at run = %q", phaseAtRun)
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseNone {
		t.Errorf("journal after a pass = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "stackr.db")); err != nil || !fi.IsDir() {
		t.Errorf("the old path is not a placeholder directory: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "db")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("db/ = %v %v", fi, err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "swap", "*.pre-*")); len(m) != 0 {
		t.Errorf("copy left after a pass: %v", m)
	}
	if got, want := calls(h.Fake), "Stop old; Run stackr-0.2.0 img:0.2.0; StopRemove old"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

// The gate fails: the layout reverses completely before the old panel starts,
// which then runs on its own spec and finds its database where it was.
func TestLayoutChangeGateFailsReversesAll(t *testing.T) {
	dir := oldInstall(t)
	h := newHook("unhealthy")
	h.onRun = func(docker.ContainerSpec) {
		// the new panel migrates the database and leaves its own wal behind
		write(t, paneldb.Path(dir), "migrated")
		write(t, paneldb.Path(dir)+"-wal", "new wal")
	}
	var atStart struct {
		main, wal       string
		placeholder, db bool
	}
	h.onStart = func(string) {
		atStart.main = read(filepath.Join(dir, "stackr.db"))
		atStart.wal = read(filepath.Join(dir, "stackr.db-wal"))
		atStart.db = exists(filepath.Join(dir, "db"))
	}
	err := leaf(h).Swap(context.Background(), oldSpec(dir))
	if err == nil {
		t.Fatal("swap passed")
	}
	if atStart.main != "old db" || atStart.wal != "old wal" || atStart.db {
		t.Errorf("at the old panel's start: %+v", atStart)
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseNone {
		t.Errorf("journal = %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "stackr.db")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the old path is not a file again: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "swap", "*.pre-*")); len(m) != 0 {
		t.Errorf("copy left after a restore: %v", m)
	}
	if got, want := calls(h.Fake), "Stop old; Run stackr-0.2.0 img:0.2.0; Logs new; StopRemove new; Start old"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
	// the old panel reads the reason where it always did, beside its database
	if _, ok := ReadFailure(paneldb.Path(dir), "0.2.0"); !ok {
		t.Error("no failure file in swap/")
	}
	if !exists(filepath.Join(dir, "stackr.db.upgrade-failed.json")) {
		t.Error("no failure file beside the old path for the old panel")
	}
}

// Docker refuses to run the new panel: nothing ran, the layout still reverses.
func TestLayoutChangeRunErrorReverses(t *testing.T) {
	dir := oldInstall(t)
	fake := dockerfake.New()
	fake.Err = map[string]error{"Run": os.ErrPermission}
	fake.Containers = []docker.Container{{ID: "old", State: "running", Labels: map[string]string{LabelRole: "panel"}}}
	err := leaf(&hook{Fake: fake}).Swap(context.Background(), oldSpec(dir))
	if err == nil {
		t.Fatal("swap passed")
	}
	if read(filepath.Join(dir, "stackr.db")) != "old db" || exists(filepath.Join(dir, "db")) || journalPhase(t, dir) != paneldb.PhaseNone {
		t.Error("the layout did not reverse")
	}
}

// A layout that cannot be undone never starts the old panel: its path would
// hold no database, and it would seed an empty one.
func TestLayoutChangeStuckRollbackDoesNotStartOld(t *testing.T) {
	dir := oldInstall(t)
	h := newHook("unhealthy")
	h.onRun = func(docker.ContainerSpec) {
		// something else landed in the placeholder: the rollback refuses to remove it
		write(t, filepath.Join(dir, "stackr.db", "stray"), "x")
	}
	err := leaf(h).Swap(context.Background(), oldSpec(dir))
	if err == nil {
		t.Fatal("swap passed")
	}
	for _, c := range h.Calls() {
		if c.Method == "Start" {
			t.Fatalf("the old panel was started: %v", c)
		}
	}
	if read(paneldb.Path(dir)) != "old db" {
		t.Error("the database left db/")
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseRollingBack {
		t.Errorf("journal = %q, want rolling-back so every boot refuses", got)
	}
	if paneldb.Check(dir, paneldb.Path(dir)) == nil {
		t.Error("the boot check lets a half-rolled-back install start")
	}
}

// Both exist: an old installer's restore ran after the layout change and wrote
// a file at the old path. The file at the old path is the newer one; the one
// in db/ is kept in swap/, never merged.
func TestLayoutChangeBothExist(t *testing.T) {
	dir := oldInstall(t)
	must(t, os.MkdirAll(filepath.Join(dir, "db"), 0o700))
	write(t, paneldb.Path(dir), "older db in db/")
	write(t, paneldb.Path(dir)+"-wal", "older wal")
	h := newHook("healthy")
	if err := leaf(h).Swap(context.Background(), oldSpec(dir)); err != nil {
		t.Fatal(err)
	}
	if got := read(paneldb.Path(dir)); got != "old db" {
		t.Errorf("db/stackr.db = %q, want the file from the old path", got)
	}
	m, _ := filepath.Glob(filepath.Join(dir, "swap", "db.superseded-*"))
	if len(m) != 2 {
		t.Fatalf("superseded files = %v", m)
	}
	var kept []string
	for _, f := range m {
		kept = append(kept, read(f))
	}
	if !strings.Contains(strings.Join(kept, "|"), "older db in db/") {
		t.Errorf("the superseded database was not kept: %v", kept)
	}
}

// A move onto a disk is in progress (a later phase): this helper leaves it alone
// and stops nothing.
func TestLayoutChangeRefusesWhileMoved(t *testing.T) {
	dir := oldInstall(t)
	must(t, paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseMoved, DiskID: "d1"}))
	h := newHook("healthy")
	if err := leaf(h).Swap(context.Background(), oldSpec(dir)); err == nil {
		t.Fatal("swap passed on a moved database")
	}
	if got := calls(h.Fake); got != "" {
		t.Errorf("docker was touched: %s", got)
	}
	if read(filepath.Join(dir, "stackr.db")) != "old db" {
		t.Error("the database moved")
	}
}

// The helper is killed at each step of the layout change. Docker runs it again,
// and the second run ends with the layout applied once, never lost.
func TestLayoutChangeCrashAtEachStep(t *testing.T) {
	steps := []struct {
		name  string
		state func(t *testing.T, dir string) // what the dead helper left
	}{
		{"journal only", func(t *testing.T, dir string) {
			must(t, paneldb.WriteJournal(dir, layoutJournal(dir)))
		}},
		{"db/ made", func(t *testing.T, dir string) {
			must(t, paneldb.WriteJournal(dir, layoutJournal(dir)))
			must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
		}},
		{"wal moved", func(t *testing.T, dir string) {
			must(t, paneldb.WriteJournal(dir, layoutJournal(dir)))
			must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
			must(t, os.Rename(filepath.Join(dir, "stackr.db-wal"), paneldb.Path(dir)+"-wal"))
		}},
		{"files moved", func(t *testing.T, dir string) {
			must(t, paneldb.WriteJournal(dir, layoutJournal(dir)))
			must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
			must(t, os.Rename(filepath.Join(dir, "stackr.db-wal"), paneldb.Path(dir)+"-wal"))
			must(t, os.Rename(filepath.Join(dir, "stackr.db"), paneldb.Path(dir)))
		}},
		{"placeholder made", func(t *testing.T, dir string) {
			must(t, paneldb.WriteJournal(dir, layoutJournal(dir)))
			must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
			must(t, os.Rename(filepath.Join(dir, "stackr.db-wal"), paneldb.Path(dir)+"-wal"))
			must(t, os.Rename(filepath.Join(dir, "stackr.db"), paneldb.Path(dir)))
			must(t, paneldb.EnsureLayout(dir))
		}},
		{"copy saved", func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
			must(t, os.Rename(filepath.Join(dir, "stackr.db-wal"), paneldb.Path(dir)+"-wal"))
			must(t, os.Rename(filepath.Join(dir, "stackr.db"), paneldb.Path(dir)))
			must(t, paneldb.EnsureLayout(dir))
			must(t, paneldb.SaveCopy(paneldb.Path(dir), filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")))
			must(t, paneldb.WriteJournal(dir, layoutJournal(dir)))
		}},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			dir := oldInstall(t)
			st.state(t, dir)
			h := newHook("healthy")
			if err := leaf(h).Swap(context.Background(), oldSpec(dir)); err != nil {
				t.Fatal(err)
			}
			if got := read(paneldb.Path(dir)); got != "old db" {
				t.Errorf("db/stackr.db = %q", got)
			}
			if got := read(paneldb.Path(dir) + "-wal"); got != "old wal" {
				t.Errorf("wal = %q", got)
			}
			if fi, err := os.Stat(filepath.Join(dir, "stackr.db")); err != nil || !fi.IsDir() {
				t.Errorf("no placeholder: %v", err)
			}
			if got := journalPhase(t, dir); got != paneldb.PhaseNone {
				t.Errorf("journal = %q", got)
			}
		})
	}
}

func layoutJournal(dir string) paneldb.Journal {
	return paneldb.Journal{Phase: paneldb.PhaseLayout, Direction: paneldb.DirLayout,
		Src: filepath.Join(dir, "stackr.db"), Dst: paneldb.Path(dir),
		Copy: filepath.Join(dir, "swap", "stackr.db.pre-0.2.0"), Container: "stackr-0.2.0"}
}

// The helper died after the new panel started, before its gate result: the
// rerun gates the recorded container again and finishes, instead of rolling
// back a panel that is only slow to show health.
func TestLayoutChangeCrashWhileGatingFinishes(t *testing.T) {
	dir := oldInstall(t)
	must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
	must(t, os.Rename(filepath.Join(dir, "stackr.db"), paneldb.Path(dir)))
	must(t, paneldb.EnsureLayout(dir))
	j := layoutJournal(dir)
	j.Phase = paneldb.PhaseStartingNew
	must(t, paneldb.WriteJournal(dir, j))
	fake := dockerfake.New()
	fake.Containers = []docker.Container{
		{ID: "old", State: "exited", Labels: map[string]string{LabelRole: "panel"}},
		{ID: "new1", Name: "stackr-0.2.0", State: "running", Labels: map[string]string{LabelRole: "panel"}},
	}
	// not "healthy" yet when the helper died; the gate sees it healthy now
	fake.Details = map[string]docker.Detail{"new1": {Running: true, Health: "healthy"}}
	h := &hook{Fake: fake, firstInspect: map[string]docker.Detail{"new1": {Running: true, Health: "starting"}}}
	l := New(h)
	l.Poll, l.Grace, l.Deadline = time.Millisecond, 50*time.Millisecond, 500*time.Millisecond
	if err := l.Swap(context.Background(), oldSpec(dir)); err != nil {
		t.Fatal(err)
	}
	if got, want := calls(fake), "StopRemove old"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if read(paneldb.Path(dir)) != "old db" || journalPhase(t, dir) != paneldb.PhaseNone {
		t.Error("the layout was undone or the journal not cleared")
	}
}

// Same crash, new panel gone: the layout is rolled back and the swap starts over.
func TestLayoutChangeCrashWhileStartingNoContainerRollsBack(t *testing.T) {
	dir := oldInstall(t)
	must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
	must(t, os.Rename(filepath.Join(dir, "stackr.db"), paneldb.Path(dir)))
	must(t, paneldb.EnsureLayout(dir))
	j := layoutJournal(dir)
	j.Phase = paneldb.PhaseStartingNew
	must(t, paneldb.SaveCopy(paneldb.Path(dir), j.Copy)) // the copy the journal names
	must(t, paneldb.WriteJournal(dir, j))
	h := newHook("unhealthy")
	var atStart string
	h.onStart = func(string) { atStart = read(filepath.Join(dir, "stackr.db")) }
	if err := leaf(h).Swap(context.Background(), oldSpec(dir)); err == nil {
		t.Fatal("swap passed")
	}
	if atStart != "old db" {
		t.Errorf("old panel started on %q", atStart)
	}
}

// The helper died after the gate passed and before it cleared the journal, and
// nobody reran it. The next Update (another version) must keep the live
// database: the live panel is gated, the stale copy dropped, the journal cleared.
func TestStuckStartingNewJournalDoesNotRollBackALivePanel(t *testing.T) {
	dir := oldInstall(t)
	must(t, os.Mkdir(filepath.Join(dir, "db"), 0o700))
	must(t, os.Rename(filepath.Join(dir, "stackr.db"), paneldb.Path(dir)))
	must(t, os.Rename(filepath.Join(dir, "stackr.db-wal"), paneldb.Path(dir)+"-wal"))
	must(t, paneldb.EnsureLayout(dir))
	cp := filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")
	must(t, paneldb.SaveCopy(paneldb.Path(dir), cp))
	// the live panel has written since the copy was taken
	write(t, paneldb.Path(dir), "live data since the upgrade")
	j := layoutJournal(dir)
	j.Phase = paneldb.PhaseStartingNew
	j.Copy = cp
	must(t, paneldb.WriteJournal(dir, j))

	fake := dockerfake.New()
	fake.RunID = "newer"
	fake.Containers = []docker.Container{{ID: "live", Name: "stackr-0.2.0", State: "running", Labels: map[string]string{LabelRole: "panel"}}}
	fake.Details = map[string]docker.Detail{"live": {Running: true, Health: "healthy"}, "newer": {Running: true, Health: "healthy"}}
	spec := docker.ContainerSpec{Name: "stackr-0.3.0", Image: "img:0.3.0",
		Env: []string{"DATA_DIR=" + dir, "DATABASE_PATH=" + paneldb.Path(dir)}}
	if err := leaf(&hook{Fake: fake}).Swap(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := read(paneldb.Path(dir)); got != "live data since the upgrade" {
		t.Errorf("the live database was replaced by the stale copy: %q", got)
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseNone {
		t.Errorf("journal = %q", got)
	}
	if exists(cp) {
		t.Error("the stale copy is still there")
	}
}

// The old panel is still running when the helper gets to the layout change:
// nothing moves.
func TestLayoutChangeRefusesWhileOldPanelRuns(t *testing.T) {
	dir := oldInstall(t)
	h := newHook("healthy")
	h.Details["old"] = docker.Detail{Running: true}
	h.stuck = true
	if err := leaf(h).Swap(context.Background(), oldSpec(dir)); err == nil {
		t.Fatal("swap passed with the old panel running")
	}
	if read(filepath.Join(dir, "stackr.db")) != "old db" || exists(filepath.Join(dir, "db")) {
		t.Error("the layout changed under a running panel")
	}
}

// laidOut is an install already on the new layout, with a live database.
func laidOut(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	must(t, paneldb.EnsureLayout(dir))
	write(t, paneldb.Path(dir), body)
	return dir
}

func newSpec(dir string) docker.ContainerSpec {
	return docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0",
		Env: []string{"DATA_DIR=" + dir, "DATABASE_PATH=" + paneldb.Path(dir)}}
}

// A copy left with no journal (the pass cleared the journal and died before it
// deleted the copy) is an earlier attempt's. A later Update of the same version
// must not restore it over newer data: it is put aside.
func TestOrphanCopyIsNeverRestored(t *testing.T) {
	for _, health := range []string{"unhealthy", "healthy"} {
		t.Run(health, func(t *testing.T) {
			dir := laidOut(t, "newer data")
			orphan := filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")
			must(t, os.MkdirAll(filepath.Dir(orphan), 0o700))
			write(t, orphan, "stale copy")
			h := newHook(health)
			var atStart string
			h.onStart = func(string) { atStart = read(paneldb.Path(dir)) }
			_ = leaf(h).Swap(context.Background(), newSpec(dir))
			if got := read(paneldb.Path(dir)); health == "healthy" && got != "newer data" {
				t.Errorf("db = %q", got)
			}
			if health == "unhealthy" && atStart != "newer data" {
				t.Errorf("old panel started on %q: the stale copy was restored", atStart)
			}
			if m, _ := filepath.Glob(filepath.Join(dir, "swap", "orphan-*")); len(m) != 1 || read(m[0]) != "stale copy" {
				t.Errorf("orphan not set aside: %v", m)
			}
			if got := journalPhase(t, dir); got != paneldb.PhaseNone {
				t.Errorf("journal = %q", got)
			}
		})
	}
}

// An ordinary swap journals its copy before the new panel starts, and a helper
// killed after that is recovered from that copy: the gate runs again on the
// recorded container; a missing container rolls back to the copy.
func TestUpdateJournalCrashRollsBackToItsCopy(t *testing.T) {
	dir := laidOut(t, "pre-upgrade")
	cp := filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")
	must(t, paneldb.SaveCopy(paneldb.Path(dir), cp))
	write(t, paneldb.Path(dir), "migrated by the dead attempt")
	must(t, paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: paneldb.DirUpdate,
		Dst: paneldb.Path(dir), Copy: cp, Container: "stackr-0.2.0", Ran: true}))
	h := newHook("unhealthy")
	var atStart string
	h.onStart = func(string) { atStart = read(paneldb.Path(dir)) }
	_ = leaf(h).Swap(context.Background(), newSpec(dir))
	if atStart != "pre-upgrade" {
		t.Errorf("old panel started on %q", atStart)
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseNone {
		t.Errorf("journal = %q", got)
	}
}

// The new panel ran and its copy is gone: no rollback, no clearing, and the old
// panel is not started on the migrated database.
func TestUpdateJournalWithoutItsCopyRefuses(t *testing.T) {
	dir := laidOut(t, "migrated")
	must(t, paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: paneldb.DirUpdate,
		Dst: paneldb.Path(dir), Copy: filepath.Join(dir, "swap", "stackr.db.pre-0.2.0"), Container: "stackr-0.2.0", Ran: true}))
	h := newHook("healthy")
	if err := leaf(h).Swap(context.Background(), newSpec(dir)); err == nil {
		t.Fatal("swap went ahead without the copy")
	}
	for _, c := range h.Calls() {
		if c.Method == "Start" || c.Method == "Run" {
			t.Fatalf("docker %s after a refused recovery", c.Method)
		}
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseStartingNew {
		t.Errorf("journal = %q, must stay", got)
	}
	if read(paneldb.Path(dir)) != "migrated" {
		t.Error("the database was touched")
	}
}

// An installed box whose configured database is missing (a custom path, or the
// canonical one gone) is refused before anything stops: never swapped without a copy.
func TestSwapRefusesAMissingDatabaseOnAnInstall(t *testing.T) {
	for name, path := range map[string]string{"custom": "elsewhere/my.db", "canonical": "db/stackr.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, paneldb.InstallFile), "{}")
			spec := newSpec(dir)
			spec.Env = []string{"DATA_DIR=" + dir, "DATABASE_PATH=" + filepath.Join(dir, path)}
			h := newHook("healthy")
			if err := leaf(h).Swap(context.Background(), spec); err == nil {
				t.Fatal("swap went ahead")
			}
			if got := calls(h.Fake); got != "" {
				t.Errorf("docker was touched: %s", got)
			}
		})
	}
}

// The helper's mount view: with DATA_DIR and a database configured elsewhere,
// it gets both folders, so it can see install.json and the database.
func TestLaunchMountsTheSpecDataDirAndACustomDatabaseFolder(t *testing.T) {
	fake := dockerfake.New()
	err := New(fake).Launch(context.Background(), docker.ContainerSpec{Image: "img:0.2.0",
		Env: []string{"DATA_DIR=/var/lib/stackr", "DATABASE_PATH=/srv/stackr.db"}})
	if err != nil {
		t.Fatal(err)
	}
	v := fake.Specs[0].Volumes
	if !slices.Contains(v, "/var/lib/stackr:/var/lib/stackr") || !slices.Contains(v, "/srv:/srv") {
		t.Errorf("volumes = %v", v)
	}
	// the canonical database adds nothing beyond the data dir
	fake = dockerfake.New()
	_ = New(fake).Launch(context.Background(), docker.ContainerSpec{Image: "img:0.2.0",
		Env: []string{"DATA_DIR=/var/lib/stackr", "DATABASE_PATH=/var/lib/stackr/db/stackr.db"}})
	if v := fake.Specs[0].Volumes; len(v) != 2 || v[1] != "/var/lib/stackr:/var/lib/stackr" {
		t.Errorf("volumes = %v", v)
	}
}

// A helper that cannot see the spec's data dir does not swap without a copy:
// it refuses before it stops anything.
func TestSwapRefusesWhenTheDataDirIsNotVisible(t *testing.T) {
	spec := docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0",
		Env: []string{"DATA_DIR=/nonexistent/stackr", "DATABASE_PATH=/nonexistent/other/my.db"}}
	h := newHook("healthy")
	if err := leaf(h).Swap(context.Background(), spec); err == nil {
		t.Fatal("swap went ahead")
	}
	if got := calls(h.Fake); got != "" {
		t.Errorf("docker was touched: %s", got)
	}
}

// Only the predecessor the journal recorded runs again after a rollback, never
// every panel-labelled container.
func TestRollbackStartsOnlyTheRecordedPredecessor(t *testing.T) {
	dir := laidOut(t, "live")
	fake := dockerfake.New()
	fake.RunID = "new"
	fake.Containers = []docker.Container{
		{ID: "old", Name: "stackr", State: "running", Labels: map[string]string{LabelRole: "panel"}},
		{ID: "ghost", Name: "stackr-0.0.1", State: "exited", Labels: map[string]string{LabelRole: "panel"}},
	}
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: "unhealthy"}}
	h := &hook{Fake: fake}
	var started []string
	h.onStart = func(id string) { started = append(started, id) }
	if err := leaf(h).Swap(context.Background(), newSpec(dir)); err == nil {
		t.Fatal("swap passed")
	}
	if len(started) != 1 || started[0] != "old" {
		t.Errorf("started %v, want only the running predecessor", started)
	}

	// a rerun: the journal records the predecessor, the other labelled panel stays down
	dir = laidOut(t, "migrated")
	cp := filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")
	must(t, os.MkdirAll(filepath.Dir(cp), 0o700))
	write(t, cp, "pre-upgrade")
	must(t, paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: paneldb.DirUpdate,
		Dst: paneldb.Path(dir), Copy: cp, Container: "stackr-0.2.0", Ran: true, Predecessors: []string{"old"}}))
	fake = dockerfake.New()
	fake.RunID = "new"
	fake.Containers = []docker.Container{
		{ID: "ghost", Name: "stackr-0.0.1", State: "exited", Labels: map[string]string{LabelRole: "panel"}},
		{ID: "old", Name: "stackr", State: "exited", Labels: map[string]string{LabelRole: "panel"}},
	}
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: "unhealthy"}}
	h = &hook{Fake: fake}
	started = nil
	h.onStart = func(id string) { started = append(started, id) }
	_ = leaf(h).Swap(context.Background(), newSpec(dir))
	if len(started) != 1 || started[0] != "old" {
		t.Errorf("rerun started %v, want only the recorded predecessor", started)
	}
}

// The predecessors are removed before the journal clears, and a removal that
// fails keeps the journal and the copy: the helper's retry finishes from the
// healthy new panel instead of leaving a startable predecessor behind.
func TestCommitRemovesPredecessorsBeforeClearingTheJournal(t *testing.T) {
	dir := laidOut(t, "live")
	fake := dockerfake.New()
	fake.RunID = "new"
	fake.Containers = []docker.Container{{ID: "old", Name: "stackr", State: "running", Labels: map[string]string{LabelRole: "panel"}}}
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: "healthy"}}
	fake.Err = map[string]error{"StopRemove(old)": errors.New("daemon busy")}
	h := &hook{Fake: fake}
	if err := leaf(h).Swap(context.Background(), newSpec(dir)); err == nil {
		t.Fatal("swap passed with the predecessor still there")
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseStartingNew {
		t.Errorf("journal = %q, must stay until the predecessor is gone", got)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")); len(m) != 1 {
		t.Error("the copy went before the journal cleared")
	}

	// the retry: the new panel (named spec.Name) is up and healthy; the helper finishes
	fake.Err = nil
	fake.Containers = []docker.Container{
		{ID: "old", Name: "stackr", State: "exited", Labels: map[string]string{LabelRole: "panel"}},
		{ID: "new", Name: "stackr-0.2.0", State: "running", Labels: map[string]string{LabelRole: "panel"}},
	}
	if err := leaf(h).Swap(context.Background(), newSpec(dir)); err != nil {
		t.Fatal(err)
	}
	if got := journalPhase(t, dir); got != paneldb.PhaseNone {
		t.Errorf("journal after the retry = %q", got)
	}
	if exists(filepath.Join(dir, "swap", "stackr.db.pre-0.2.0")) {
		t.Error("copy left after the commit")
	}
	var removed bool
	for _, c := range fake.Calls() {
		if c.Method == "StopRemove" && len(c.Args) > 0 && c.Args[0] == "old" {
			removed = true
		}
	}
	if !removed {
		t.Error("the predecessor was never removed")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
