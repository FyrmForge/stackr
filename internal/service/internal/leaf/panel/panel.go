// Package panel is stackrd's own container: find it, pull a new image, hand
// the swap to a one-shot helper, and the swap itself (run by that helper,
// since a process cannot outlive stopping its own container).
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/paneldb"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Docker is the slice of the daemon this leaf uses.
type Docker interface {
	Run(ctx context.Context, spec docker.ContainerSpec) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	StopRemove(ctx context.Context, id string) error
	List(ctx context.Context, labels map[string]string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	Pull(ctx context.Context, ref, auth string, log io.Writer) error
	LocalDigest(ctx context.Context, ref string) (string, error)
	Tag(ctx context.Context, src, dst string) error
}

const (
	LabelRole = "stackr.role"
	// SpecEnv carries the new panel's spec into the helper, as JSON.
	SpecEnv = installspec.SwapSpecEnv
)

// RetryEnv marks a helper that Docker restarts when it dies (set by Launch).
const RetryEnv = "STACKR_SWAP_DOCKER_RETRY"

// ErrRolledBack wraps a swap error when the old panel is running again: the
// helper has nothing left to retry (it exits 0 on it).
var ErrRolledBack = errors.New("rolled back to the old panel")

type Leaf struct {
	docker Docker
	// Gate for the new panel: migrations may run before it answers.
	Poll, Grace, Deadline time.Duration
}

func New(d Docker) *Leaf {
	return &Leaf{
		docker:   d,
		Poll:     time.Second,
		Grace:    10 * time.Second,
		Deadline: 3 * time.Minute,
	}
}

// Pull fetches a panel image (public, anonymous). One already here is
// kept: a release tag never changes, and a box can be fed images by hand.
func (l *Leaf) Pull(ctx context.Context, image string, log io.Writer) error {
	if _, err := l.docker.LocalDigest(ctx, image); err == nil {
		return nil
	}
	return l.docker.Pull(ctx, image, "", log)
}

// Tag points dst at the local image src: the digest a release declares,
// pulled, becomes the tag the rest of the upgrade runs by.
func (l *Leaf) Tag(ctx context.Context, src, dst string) error {
	return l.docker.Tag(ctx, src, dst)
}

// Digest is the registry digest the local image was pulled as ("" when it
// has none, a hand-fed or locally built one).
func (l *Leaf) Digest(ctx context.Context, image string) (string, error) {
	return l.docker.LocalDigest(ctx, image)
}

// Launch starts the helper that swaps the panel to spec: the new image
// running `stackrd upgrade-swap` with the socket. Refused while a helper
// still runs, which is the "already running" guard across panel restarts.
func (l *Leaf) Launch(ctx context.Context, spec docker.ContainerSpec) error {
	busy, err := l.docker.List(ctx, map[string]string{LabelRole: "upgrader"})
	if err != nil {
		return err
	}
	for _, c := range busy {
		if c.State == "running" {
			return errs.Conflictf("an upgrade is already running")
		}
		_ = l.docker.StopRemove(ctx, c.ID) // a finished helper from last time
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	vols := []string{"/var/run/docker.sock:/var/run/docker.sock"}
	if db := dbPath(spec); db != "" {
		// The swap copies the database aside and keeps the move journal, so the helper needs the data dir at the panel's path.
		dir := paneldb.DataDirOf(db)
		vols = append(vols, dir+":"+dir)
	}
	_, err = l.docker.Run(ctx, docker.ContainerSpec{
		// The image's entrypoint is /stackrd; Cmd is only its argument.
		Name:    "stackr-upgrader",
		Image:   spec.Image,
		Cmd:     []string{"upgrade-swap"},
		Env:     []string{SpecEnv + "=" + string(body), RetryEnv + "=1"},
		Labels:  map[string]string{LabelRole: "upgrader"},
		Volumes: vols,
		// The gate dials the new panel's listen address, which is the host's.
		HostNetwork: true,
		// A helper that dies mid-swap (killed, or the daemon restarted) runs
		// again, and Swap starts over from whatever the first try left. A
		// swap that failed cleanly and rolled back also exits non-zero, so
		// the retries are bounded: three, then the old panel stays up.
		Restart: "on-failure:3",
	})
	return err
}

// SpecFromEnv reads the helper's spec back.
func SpecFromEnv() (docker.ContainerSpec, error) {
	var s docker.ContainerSpec
	raw := os.Getenv(SpecEnv)
	if raw == "" {
		return s, errors.New(SpecEnv + " is not set")
	}
	return s, json.Unmarshal([]byte(raw), &s)
}

// dbPath is the database file the spec's panel opens ("" = unknown).
func dbPath(spec docker.ContainerSpec) string {
	for _, e := range spec.Env {
		if v, ok := strings.CutPrefix(e, "DATABASE_PATH="); ok {
			return v
		}
	}
	return ""
}

// copyName is the per-upgrade copy of the database, named after the target
// and kept in swap/ (never beside the database).
func copyName(db string, spec docker.ContainerSpec) string {
	v := spec.Image[strings.LastIndex(spec.Image, ":")+1:]
	return filepath.Join(paneldb.SwapDirOf(db), filepath.Base(db)+".pre-"+strings.ReplaceAll(v, "/", "_"))
}

// Leftovers lists swap copies still in swap/: a helper that died mid-swap
// leaves one. Boot only reports them; they are never deleted there.
func Leftovers(db string) []string {
	m, _ := filepath.Glob(filepath.Join(paneldb.SwapDirOf(db), filepath.Base(db)+".pre-*"))
	return m
}

// Swap replaces the running panel with spec: stop the old one (kept), copy
// its database aside, run the new one, gate it like a tile; pass removes the
// old and the copy, fail removes the new, puts the database back and starts
// the old again. Plain containers have no Swarm to roll back a failed swap,
// so this is that rollback; the copy is needed because the new panel may
// have migrated the database past what the old one can open.
//
// The helper is always the new image, so it also owns the layout change: the
// first Update after it shipped is built by an old panel whose spec still says
// DATABASE_PATH=<DataDir>/stackr.db. The files move into db/, the old path
// becomes a placeholder directory, and the spec handed to the new panel says
// <DataDir>/db/stackr.db. A failed gate reverses all of it before the old
// panel starts, and a layout that cannot be reversed never starts it.
func (l *Leaf) Swap(ctx context.Context, spec docker.ContainerSpec) error {
	old, err := l.docker.List(ctx, map[string]string{LabelRole: "panel"})
	if err != nil {
		return err
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[LabelRole] = "panel"
	db := dbPath(spec)
	origDB := db
	dataDir := specEnv(spec, "DATA_DIR")
	// Only an installed panel (DATA_DIR in its spec) has the layout.
	layout := dataDir != "" && (db == paneldb.Legacy(dataDir) || db == paneldb.Path(dataDir))
	if layout {
		_, e1 := os.Lstat(paneldb.Legacy(dataDir))
		_, e2 := os.Stat(paneldb.Path(dataDir))
		layout = e1 == nil || e2 == nil // neither visible to the helper: the no-copy case below
	}
	// On an installed box (its install.json is visible) the configured database
	// must be there, whatever the path: a missing one is a loss to refuse, never a
	// "not visible" to swap past without a copy.
	if !layout && db != "" && dataDir != "" {
		if _, ierr := os.Stat(filepath.Join(dataDir, paneldb.InstallFile)); ierr == nil {
			if _, err := os.Stat(db); err != nil {
				return fmt.Errorf("the database %s is missing on an installed box (%v): refusing to swap without it", db, err)
			}
		}
	}
	var j paneldb.Journal
	if layout {
		db = paneldb.Path(dataDir)
		spec.Env = setEnv(spec.Env, "DATABASE_PATH", db)
		if j, err = paneldb.ReadJournal(dataDir); err != nil {
			return err
		}
		if j.Phase != paneldb.PhaseNone && j.Phase != paneldb.PhaseFresh && j.Direction != paneldb.DirLayout && j.Direction != paneldb.DirUpdate {
			// A move onto a node disk is in progress: not this helper's to resolve.
			return fmt.Errorf("the database is in phase %q (%s): run stackr-install on this node", j.Phase, j.Direction)
		}
	}
	// A failure file of an earlier upgrade is not this trial's.
	if db != "" {
		for _, f := range failureFiles(db, origDB) {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("upgrade: old failure file not removed", "err", err)
			}
		}
	}
	// A rerun (the helper died and Docker started it again) finds the new
	// panel of the first try, named spec.Name. It is not an old panel: it
	// goes, and the database copy of the first try goes back, so the swap
	// starts again from the panel that was running before.
	var stale []docker.Container
	old = slices.DeleteFunc(old, func(c docker.Container) bool {
		if c.Name == spec.Name {
			stale = append(stale, c)
		}
		return c.Name == spec.Name
	})
	// A journal at starting-new or gating names a new panel that may be live: a
	// helper that died after its gate passed and before it cleared the journal,
	// whichever version it was for. It is gated again and kept when it passes,
	// never rolled back for slow health; only a missing one, or one that fails
	// the gate, goes on to the rollback below.
	if layout && (j.Phase == paneldb.PhaseStartingNew || j.Phase == paneldb.PhaseGating) && j.Container != "" {
		for _, c := range append(slices.Clone(old), stale...) {
			if c.Name != j.Container {
				continue
			}
			if d, err := l.docker.Inspect(ctx, c.ID); err != nil || !d.Running || l.gate(ctx, c.ID, spec) != nil {
				continue
			}
			// Predecessors go first, and must go: a journal cleared with one left
			// behind could see it started by a later rollback.
			var rest []docker.Container
			for _, o := range append(slices.Clone(old), stale...) {
				if o.ID == c.ID {
					continue
				}
				if err := l.docker.StopRemove(ctx, o.ID); err != nil {
					return fmt.Errorf("remove the predecessor %s: %w", o.Name, err)
				}
			}
			if err := paneldb.ClearJournal(dataDir); err != nil {
				return err
			}
			paneldb.DropCopy(j.Copy) // only after the journal is clear: a crash before it still has its copy
			j = paneldb.Journal{Phase: paneldb.PhaseNone}
			if c.Name == spec.Name { // this very swap: done
				return nil
			}
			// Another version's panel is the live one: this swap goes on from it.
			for _, o := range old {
				if o.ID == c.ID {
					rest = append(rest, o)
				}
			}
			old, stale = rest, nil
			break
		}
	}
	// The first try died after its gate passed: the new panel is up and
	// healthy, and it has been writing. Finish the pass instead of undoing it.
	if len(stale) == 1 {
		if d, err := l.docker.Inspect(ctx, stale[0].ID); err == nil && d.Running {
			pass := d.Health == "healthy"
			if pass {
				for _, c := range old {
					if err := l.docker.StopRemove(ctx, c.ID); err != nil {
						return fmt.Errorf("remove the predecessor %s: %w", c.Name, err)
					}
				}
				if layout {
					if err := paneldb.ClearJournal(dataDir); err != nil {
						return err
					}
				}
				if db != "" {
					paneldb.DropCopy(copyName(db, spec))
				}
				return nil
			}
		}
	}
	// The predecessors a rollback may start: the ones the journal recorded, else
	// the ones running now, else (a rerun, they are stopped) all that are left.
	// Never every panel-labelled container.
	startList := old
	if len(j.Predecessors) > 0 {
		startList = slices.DeleteFunc(slices.Clone(old), func(c docker.Container) bool { return !slices.Contains(j.Predecessors, c.ID) })
	} else if run := slices.DeleteFunc(slices.Clone(old), func(c docker.Container) bool { return c.State != "running" }); len(run) > 0 {
		startList = run
	}
	predIDs := make([]string, 0, len(startList))
	for _, c := range startList {
		predIDs = append(predIDs, c.ID)
	}
	if len(startList) == 0 && len(j.Predecessors) > 0 {
		return errors.New("the predecessor panel the journal recorded is gone: not starting any other panel")
	}
	for _, c := range old {
		if err := l.docker.Stop(ctx, c.ID); err != nil {
			return fmt.Errorf("stop old panel: %w", err)
		}
	}
	for _, c := range stale {
		if err := l.docker.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove the new panel of the first try: %w", err)
		}
	}
	back := func(cause error) error {
		for _, c := range startList {
			if err := l.docker.Start(ctx, c.ID); err != nil {
				return errors.Join(cause, fmt.Errorf("start old panel: %w", err))
			}
		}
		return fmt.Errorf("%w: %w", ErrRolledBack, cause)
	}
	// A layout change the first try left unfinished goes back first. When it
	// cannot, the old panel is not started: its path may hold no database.
	if layout && j.Phase != paneldb.PhaseNone {
		if err := paneldb.Recover(dataDir); err != nil {
			return fmt.Errorf("undo the unfinished layout change (the old panel is not started): %w", err)
		}
	}
	if _, err := os.Stat(db); db != "" && err != nil && !layout {
		// A helper launched by a panel built before the data-dir mount cannot see the file.
		slog.Warn("upgrade: database not visible to the helper, swapping without a copy", "file", db, "err", err)
		db = ""
	}
	var cp string
	if db != "" {
		cp = copyName(db, spec)
		if _, err := os.Stat(cp); err == nil {
			if layout {
				// Recover has already used the copy the journal names. One that is
				// left with no journal is an earlier attempt's: it may be older than
				// the database, so it is put aside, never restored.
				if err := paneldb.AsideCopy(dataDir, cp); err != nil {
					return back(fmt.Errorf("put aside an old copy: %w", err))
				}
			} else if err := paneldb.RestoreCopy(db, cp); err != nil { // left by the first try
				return back(fmt.Errorf("restore the database of the first try: %w", err))
			}
		}
	}
	applied := false
	if layout {
		// Confirmed exited by its container state, not a timer: nothing may hold the files.
		for _, c := range old {
			if d, err := l.docker.Inspect(ctx, c.ID); err != nil || d.Running {
				return back(fmt.Errorf("the old panel %s is not confirmed exited (%v): the database layout is left alone", c.Name, err))
			}
		}
		if applied, err = paneldb.ApplyLayout(dataDir, cp, spec.Name, predIDs...); err != nil {
			if rerr := paneldb.Recover(dataDir); rerr != nil {
				return errors.Join(fmt.Errorf("change the database layout: %w", err), fmt.Errorf("undo it (the old panel is not started): %w", rerr))
			}
			return back(fmt.Errorf("change the database layout: %w", err))
		}
	}
	undo := func(restoreCopy bool, cause error) error {
		return l.undone(undoArgs{applied: applied, layout: layout, dataDir: dataDir, db: db, cp: cp, restoreCopy: restoreCopy}, back, cause)
	}
	if db != "" {
		if err := paneldb.SaveCopy(db, cp); err != nil {
			return undo(false, fmt.Errorf("copy database: %w", err))
		}
	}
	if layout {
		// From here the new panel may take writes: recovery re-runs the gate instead of rolling back.
		// The journal names this attempt's copy, the only one a recovery may restore.
		dir := paneldb.DirUpdate
		if applied {
			dir = paneldb.DirLayout
		}
		if err := paneldb.WriteJournal(dataDir, paneldb.Journal{Phase: paneldb.PhaseStartingNew, Direction: dir,
			Src: paneldb.Legacy(dataDir), Dst: db, Copy: cp, Container: spec.Name, Ran: true, Image: spec.Image, Predecessors: predIDs}); err != nil {
			return undo(false, fmt.Errorf("journal: %w", err))
		}
	}
	id, err := l.docker.Run(ctx, spec)
	if installspec.DisksBindRefused(err) && slices.Contains(spec.Volumes, installspec.DisksBindVolume) {
		// Docker refuses the bind on a host whose mounts are private. A
		// container that failed to start is already removed, so the name is free.
		slog.Warn("panel started without the disks mount: this host's mounts do not allow it", "err", err)
		spec.Volumes = installspec.WithoutDisksBind(spec.Volumes)
		id, err = l.docker.Run(ctx, spec)
	}
	if err != nil {
		// The new panel never ran: the database is untouched.
		return undo(false, fmt.Errorf("run new panel: %w", err))
	}
	if err = l.gate(ctx, id, spec); err != nil {
		if db != "" {
			l.recordFailure(ctx, id, db, origDB, spec.Image[strings.LastIndex(spec.Image, ":")+1:], err) // before the container, and its log, go
		}
		_ = l.docker.StopRemove(ctx, id)
		return undo(db != "", fmt.Errorf("new panel: %w", err))
	}
	// The predecessors go before the journal clears: with one left behind, a
	// later rollback could start it. A failure here leaves the journal, and the
	// helper's retry finishes from the healthy new panel.
	for _, c := range old {
		if err := l.docker.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove the predecessor %s: %w", c.Name, err)
		}
	}
	if layout {
		if err := paneldb.ClearJournal(dataDir); err != nil {
			return err
		}
	}
	if db != "" {
		paneldb.DropCopy(cp) // only after the journal is clear
	}
	return nil
}

type undoArgs struct {
	applied     bool   // the layout change ran in this swap
	layout      bool   // the swap keeps a journal
	dataDir, db string // db: the path the new panel uses
	cp          string // "" = no copy was made
	restoreCopy bool   // the new panel ran: the copy goes over the database
}

// undone puts the database back where the old panel looks for it, then starts
// the old panel. It never starts it on a database it could not put back.
func (l *Leaf) undone(a undoArgs, back func(error) error, cause error) error {
	var err error
	switch {
	case a.applied:
		// The layout goes back first, then the copy goes to the path the old panel uses.
		// The copy stays until the journal is cleared: a crash before then repeats the restore from it.
		if err = paneldb.RollbackLayout(a.dataDir); err == nil && a.restoreCopy {
			err = paneldb.RestoreCopy(paneldb.Legacy(a.dataDir), a.cp)
		}
		if err == nil {
			err = paneldb.ClearJournal(a.dataDir)
		}
		if err == nil && a.cp != "" {
			paneldb.DropCopy(a.cp)
		}
	case a.cp != "" && a.restoreCopy:
		if err = paneldb.RestoreCopy(a.db, a.cp); err == nil && a.layout {
			err = paneldb.ClearJournal(a.dataDir)
		}
		if err == nil {
			paneldb.DropCopy(a.cp)
		}
	case a.cp != "":
		if a.layout {
			err = paneldb.ClearJournal(a.dataDir)
		}
		if err == nil {
			paneldb.DropCopy(a.cp)
		}
	}
	if err != nil {
		// Starting the old panel on a migrated database would only crash it.
		return errors.Join(cause, fmt.Errorf("put the database back from %s (the old panel is not started): %w", a.cp, err))
	}
	return back(cause)
}

// gate is the new panel's health gate: docker.Healthy, then the HTTP probe.
func (l *Leaf) gate(ctx context.Context, id string, spec docker.ContainerSpec) error {
	err := docker.Healthy(ctx, l.docker.Inspect, id, l.Poll, l.Grace, l.Deadline)
	if err == nil {
		err = l.probe(ctx, spec)
	}
	return err
}

func specEnv(spec docker.ContainerSpec, key string) string {
	for _, e := range spec.Env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

func setEnv(env []string, key, val string) []string {
	out := slices.Clone(env)
	for i, e := range out {
		if strings.HasPrefix(e, key+"=") {
			out[i] = key + "=" + val
			return out
		}
	}
	return append(out, key+"="+val)
}

// probe asks the new panel's /api/health over HTTP until it answers 200 or
// the deadline passes: the panel has no HEALTHCHECK, so the container gate
// alone passes a panel that runs but serves nothing. The address is the
// spec's HOST and PORT; a spec with no PORT (a test's) is not probed.
func (l *Leaf) probe(ctx context.Context, spec docker.ContainerSpec) error {
	var host, port string
	for _, e := range spec.Env {
		if v, ok := strings.CutPrefix(e, "HOST="); ok {
			host = v
		}
		if v, ok := strings.CutPrefix(e, "PORT="); ok {
			port = v
		}
	}
	if port == "" {
		return nil
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/api/health"
	ctx, cancel := context.WithTimeout(ctx, l.Deadline)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the panel did not answer %s with 200 before the deadline", url)
		case <-time.After(l.Poll):
		}
	}
}

// Failure is why the new panel failed its trial. The helper writes it before
// it removes the container, since the log goes with it; the old panel reads
// it back at boot (TakeFailure). Only the exit code and the log tail are
// kept, never the container's env.
type Failure struct {
	Tag      string `json:"tag"` // the image tag that was on trial
	Reason   string `json:"reason"`
	ExitCode int    `json:"exit_code"`
	Log      string `json:"log"`
}

// failureTail is how many log lines of the new panel are kept.
const failureTail = 100

// FailureFile is where the helper leaves the Failure: swap/, on the system
// disk, never beside the database (the one dir both containers see).
func FailureFile(db string) string {
	return filepath.Join(paneldb.SwapDirOf(db), filepath.Base(db)+".upgrade-failed.json")
}

// failureFiles is every place a trial's Failure may be left: swap/, and beside
// the old database path when the layout change ran, which is where the old
// panel (built before it) looks for it after a rollback.
func failureFiles(db, origDB string) []string {
	fs := []string{FailureFile(db)}
	if origDB != "" && origDB != db {
		fs = append(fs, origDB+".upgrade-failed.json")
	}
	return fs
}

func (l *Leaf) recordFailure(ctx context.Context, id, db, origDB, tag string, cause error) {
	f := Failure{Tag: tag, Reason: cause.Error()}
	if d, err := l.docker.Inspect(ctx, id); err == nil && !d.Running {
		f.ExitCode = d.ExitCode
	}
	f.Log, _ = l.docker.Logs(ctx, id, failureTail)
	if b, err := json.Marshal(f); err == nil {
		_ = os.MkdirAll(paneldb.SwapDirOf(db), 0o700)
		for _, name := range failureFiles(db, origDB) {
			if err := os.WriteFile(name, b, 0o600); err != nil {
				slog.Warn("upgrade: failure file not written", "err", err)
			}
		}
	}
}

// ReadFailure is the helper's Failure for the trial of tag; false = none was
// left, or it is another trial's. It stays on disk until RemoveFailure.
func ReadFailure(db, tag string) (Failure, bool) {
	var f Failure
	b, err := os.ReadFile(FailureFile(db))
	if err != nil || json.Unmarshal(b, &f) != nil || f.Tag != tag {
		return Failure{}, false
	}
	return f, true
}

// RemoveFailure deletes the helper's Failure file.
func RemoveFailure(db string) {
	if err := os.Remove(FailureFile(db)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("upgrade: failure file not removed", "err", err)
	}
}
