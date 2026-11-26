package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/paneldb"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// The panel database moves onto a node disk and back. The helper (this code,
// run by `stackrd upgrade-swap` with MoveEnv set) does it with the panel
// stopped: a panel cannot move its own database. Every file operation under
// <DataDir>/db goes through the privileged disk helper (DiskRunner); this
// process only keeps move.journal, the Docker calls and the order. The journal
// is written before each external change (an intent phase) and after it, and
// every resume reads it and nothing else, so a crash at any row of the RFC's
// tables ends in one side holding the data and the other clean.

// MoveEnv carries the move's parameters into the helper, as JSON.
const MoveEnv = "STACKR_PANEL_MOVE"

// Move is what the helper is told to do.
type Move struct {
	Dir       string `json:"dir"` // paneldb.DirIn or paneldb.DirBack
	MoveID    string `json:"move_id"`
	DiskID    string `json:"disk_id"`
	Folder    string `json:"folder"` // host path of the disk folder
	InstallID string `json:"install_id"`
	Stamp     string `json:"stamp"` // UTC time, the name of the copy this move leaves
}

// Names of the swap/ files a move uses.
const (
	movingName   = "stackr.db.moving"
	incomingName = "stackr.db.incoming"
)

// DiskRunner runs one request of the privileged disk helper on the node.
type DiskRunner interface {
	Disk(ctx context.Context, r hostdisk.Request) (hostdisk.Result, error)
}

// errCrash is what a test's crash hook returns: the helper "died" there, and
// nothing after it ran (no rollback either).
var errCrash = errors.New("simulated crash")

// MoveFromEnv reads the move back; false = this helper does an ordinary swap.
func MoveFromEnv() (Move, bool, error) {
	raw := os.Getenv(MoveEnv)
	if raw == "" {
		return Move{}, false, nil
	}
	var mv Move
	return mv, true, json.Unmarshal([]byte(raw), &mv)
}

// LaunchMove starts the helper for a database move: the ordinary helper with
// MoveEnv set. spec is the panel's own spec on its own image; spec.Name is the
// name the new panel takes.
func (l *Leaf) LaunchMove(ctx context.Context, spec docker.ContainerSpec, mv Move) error {
	b, err := json.Marshal(mv)
	if err != nil {
		return err
	}
	return l.launch(ctx, spec, MoveEnv+"="+string(b))
}

// HelperDisk runs a request in a privileged container of the stackr image
// with the host's / at /host, a fresh one per request (so it sees the host's
// mounts as they are now).
type HelperDisk struct {
	D     DiskDocker
	Image string
	Wait  time.Duration // 0 = 10 minutes (a copy of a big database)
}

// DiskDocker is the slice of the daemon the disk helper runner uses.
type DiskDocker interface {
	Run(ctx context.Context, spec docker.ContainerSpec) (string, error)
	Wait(ctx context.Context, id string) (int, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	StopRemove(ctx context.Context, id string) error
}

func (h HelperDisk) Disk(ctx context.Context, r hostdisk.Request) (hostdisk.Result, error) {
	wait := h.Wait
	if wait == 0 {
		wait = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var b [3]byte
	_, _ = rand.Read(b[:])
	name := "stackr-disk-" + hex.EncodeToString(b[:])
	deadline := strconv.FormatInt(time.Now().Add(wait+time.Minute).Unix(), 10)
	id, err := h.D.Run(ctx, docker.ContainerSpec{
		Name: name, Image: h.Image, Cmd: []string{"host-run", "disk", hostdisk.EncodeRequest(r)},
		Labels:     map[string]string{LabelRole: "disk-helper", "stackr.disk-deadline": deadline},
		Privileged: true, PidHost: true, HostNetwork: true, Volumes: []string{"/:/host"}, Restart: "no",
	})
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if id == "" {
			id = name
		}
		_ = h.D.StopRemove(cctx, id)
	}()
	if err != nil {
		return hostdisk.Result{}, err
	}
	code, err := h.D.Wait(ctx, id)
	if err != nil {
		return hostdisk.Result{}, fmt.Errorf("disk helper: %w", err)
	}
	out, err := h.D.Logs(ctx, id, 50)
	if err != nil {
		return hostdisk.Result{}, fmt.Errorf("disk helper: %w", err)
	}
	res, ok := hostdisk.ParseResult([]byte(out))
	if !ok || code != 0 {
		return hostdisk.Result{}, fmt.Errorf("the disk helper crashed (exit %d): %s", code, strings.TrimSpace(out))
	}
	return res, nil
}

// diskRunner is the injected runner, or the real one on spec's image.
func (l *Leaf) diskRunner(spec docker.ContainerSpec) DiskRunner {
	if l.Disk != nil {
		return l.Disk
	}
	return HelperDisk{D: l.docker, Image: spec.Image}
}

// mover is one run of a move or of a moved install's Update.
type mover struct {
	l       *Leaf
	disk    DiskRunner
	dataDir string
	spec    docker.ContainerSpec
	j       paneldb.Journal
}

// op runs one paneldb request. A refusal is returned as its *hostdisk.Error.
func (m *mover) op(ctx context.Context, name, mounted string, from, to hostdisk.DBFile, optional ...bool) (*hostdisk.DBResult, error) {
	res, err := m.disk.Disk(ctx, hostdisk.Request{Op: name, Kind: "paneldb", DB: &hostdisk.DBReq{
		DataDir: m.dataDir, DiskID: m.j.DiskID, InstallID: m.j.InstallID, Mounted: mounted, From: from, To: to,
		Optional: len(optional) > 0 && optional[0],
	}})
	if err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, fmt.Errorf("%s: %w", name, res.Error)
	}
	if res.DB == nil {
		return &hostdisk.DBResult{}, nil
	}
	return res.DB, nil
}

var none hostdisk.DBFile

func file(area, name string) hostdisk.DBFile { return hostdisk.DBFile{Area: area, Name: name} }

func (m *mover) set(phase string) error {
	m.j.Phase = phase
	return paneldb.WriteJournal(m.dataDir, m.j)
}

func (m *mover) crash(step string) error {
	if m.l.crashAt != nil {
		return m.l.crashAt(step)
	}
	return nil
}

// Move does the move the journal and mv describe, resuming whatever an
// earlier run left. A clean rollback returns ErrRolledBack (the old panel runs
// again); a crash-style error leaves the journal where it is.
func (l *Leaf) Move(ctx context.Context, spec docker.ContainerSpec, mv Move) error {
	dataDir := specEnv(spec, "DATA_DIR")
	if dataDir == "" {
		return errors.New("the panel spec has no DATA_DIR: not an installed panel")
	}
	if mv.Dir != paneldb.DirIn && mv.Dir != paneldb.DirBack {
		return fmt.Errorf("unknown move direction %q", mv.Dir)
	}
	if paneldb.Path(dataDir) != specEnv(spec, "DATABASE_PATH") {
		return fmt.Errorf("the panel's database path is %s, not %s: a move needs the standard layout", specEnv(spec, "DATABASE_PATH"), paneldb.Path(dataDir))
	}
	j, err := paneldb.ReadJournal(dataDir)
	if err != nil {
		return err
	}
	m := &mover{l: l, disk: l.diskRunner(spec), dataDir: dataDir, spec: spec, j: j}
	if spec.Labels == nil {
		m.spec.Labels = map[string]string{}
	}
	m.spec.Labels[LabelRole] = "panel"
	switch {
	case j.Direction == mv.Dir && j.Phase == paneldb.PhaseRollingBack:
		return m.rollback(ctx, errors.New("an earlier try was rolling back"))
	case j.Direction == mv.Dir && j.Phase != paneldb.PhaseNone && j.Phase != paneldb.PhaseMoved:
		return m.resume(ctx, mv)
	case j.Phase == paneldb.PhaseMoved && mv.Dir == paneldb.DirIn:
		// a finished move-in: the helper retry that comes after the pass
		if m.j.Direction == paneldb.DirIn && m.j.MoveID == mv.MoveID {
			return nil
		}
		return errors.New("the database is already on a node disk")
	case j.Phase == paneldb.PhaseMoved && mv.Dir == paneldb.DirBack:
		return m.begin(ctx, mv)
	case j.Phase == paneldb.PhaseNone && mv.Dir == paneldb.DirIn:
		return m.begin(ctx, mv)
	case j.Phase == paneldb.PhaseNone && mv.Dir == paneldb.DirBack && j.MoveID == mv.MoveID:
		return nil // finished: the retry after the pass
	}
	return fmt.Errorf("move.journal is in phase %q (%s): not starting a move %s. Run stackr-install on this node", j.Phase, j.Direction, mv.Dir)
}

// panels lists the panel containers now, and the ones a rollback may start:
// the recorded predecessors, never every panel-labelled container.
func (m *mover) panels(ctx context.Context) (all, pred []docker.Container, err error) {
	all, err = m.l.docker.List(ctx, map[string]string{LabelRole: "panel"})
	if err != nil {
		return nil, nil, err
	}
	for _, c := range all {
		if c.Name != m.spec.Name && slices.Contains(m.j.Predecessors, c.ID) {
			pred = append(pred, c)
		}
	}
	return all, pred, nil
}

// begin starts a fresh move.
func (m *mover) begin(ctx context.Context, mv Move) error {
	all, _, err := m.panels(ctx)
	if err != nil {
		return err
	}
	var running []docker.Container
	for _, c := range all {
		if c.Name == m.spec.Name {
			return fmt.Errorf("a panel named %s exists: not starting a move", c.Name)
		}
		if c.State == "running" {
			running = append(running, c)
		}
	}
	if len(running) != 1 {
		return fmt.Errorf("expected one running panel, found %d: not starting a move", len(running))
	}
	ids := []string{running[0].ID}
	mounted := "no"
	if mv.Dir == paneldb.DirBack {
		mounted = "yes"
	}
	// db/ must be what the direction expects, judged on the host.
	if _, err := m.op(ctx, "hash", mounted, file(hostdisk.AreaDB, paneldb.FileName), none); err != nil {
		return err
	}
	if prev := m.j; prev.Phase == paneldb.PhaseMoved && (prev.DiskID != mv.DiskID || prev.InstallID != mv.InstallID) {
		return errors.New("the move names another disk or install than move.journal does")
	}
	m.j = paneldb.Journal{Phase: paneldb.PhasePrepared, Direction: mv.Dir, MoveID: mv.MoveID, DiskID: mv.DiskID, Folder: mv.Folder,
		InstallID: mv.InstallID, Container: m.spec.Name, Image: m.spec.Image, Predecessors: ids,
		OldCopy: paneldb.FileName + ".moved-" + mv.Stamp + "-" + short(mv.MoveID),
		Src:     paneldb.Path(m.dataDir)}
	if mv.Dir == paneldb.DirIn {
		if _, err := m.op(ctx, "place", "", none, none); err != nil {
			return err
		}
	}
	if err := paneldb.WriteJournal(m.dataDir, m.j); err != nil {
		return err
	}
	if err := m.crash("prepared"); err != nil {
		return err
	}
	return m.forward(ctx)
}

func short(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// resume continues a move the journal names. The rule: before the new panel
// starts the old data is untouched, so a failure rolls back; from starting-new
// on the new panel may have taken writes, so the gate is run again on the
// recorded container and decides.
func (m *mover) resume(ctx context.Context, mv Move) error {
	if m.j.MoveID != mv.MoveID {
		return fmt.Errorf("move.journal belongs to another move (%s): run stackr-install on this node", m.j.MoveID)
	}
	switch m.j.Phase {
	case paneldb.PhaseStartingNew, paneldb.PhaseGating:
		return m.regate(ctx)
	}
	return m.forward(ctx)
}

// forward runs the stages the journal's phase has not passed.
func (m *mover) forward(ctx context.Context) error {
	var err error
	if m.j.Direction == paneldb.DirIn {
		err = m.moveIn(ctx)
	} else {
		err = m.moveBack(ctx)
	}
	if err != nil && !errors.Is(err, errCrash) && !errors.Is(err, ErrRolledBack) {
		return m.rollback(ctx, err)
	}
	return err
}

func (m *mover) stopOld(ctx context.Context) error {
	_, pred, err := m.panels(ctx)
	if err != nil {
		return err
	}
	for _, c := range pred {
		if err := m.l.docker.Stop(ctx, c.ID); err != nil {
			return fmt.Errorf("stop the old panel: %w", err)
		}
	}
	// confirmed exited by its container state, not a timer
	for _, c := range pred {
		if d, err := m.l.docker.Inspect(ctx, c.ID); err != nil || d.Running {
			return fmt.Errorf("the old panel %s is not confirmed exited (%v)", c.Name, err)
		}
	}
	return nil
}

var dbMain = file(hostdisk.AreaDB, paneldb.FileName)

func (m *mover) moveIn(ctx context.Context) error {
	j := &m.j
	folderDB := file(hostdisk.AreaFolder, paneldb.FileName)
	marker := file(hostdisk.AreaFolder, hostdisk.PanelDBMarker)
	moving, movingWal := file(hostdisk.AreaSwap, movingName), file(hostdisk.AreaSwap, movingName+"-wal")
	if j.Phase == paneldb.PhasePrepared {
		if err := m.stopOld(ctx); err != nil {
			return err
		}
		if err := m.crash("stopped"); err != nil {
			return err
		}
		// the marker goes first: a copy with no marker is never trusted
		if _, err := m.op(ctx, "rm", "", marker, none); err != nil {
			return err
		}
		if _, err := m.op(ctx, "checkpoint", "no", dbMain, none); err != nil {
			return err
		}
		if err := m.crash("checkpointed"); err != nil {
			return err
		}
		cp, err := m.op(ctx, "copy", "no", dbMain, folderDB)
		if err != nil {
			return err
		}
		if err := m.crash("copy-renamed"); err != nil {
			return err
		}
		src, err := m.op(ctx, "hash", "no", dbMain, none)
		if err != nil {
			return err
		}
		if !src.Exists || src.SHA256 != cp.SHA256 {
			return fmt.Errorf("the copy's hash %s is not the source's %s", cp.SHA256, src.SHA256)
		}
		if _, err := m.op(ctx, "integrity", "", folderDB, none); err != nil {
			return err
		}
		if _, err := m.op(ctx, "marker-write", "", none, none); err != nil {
			return err
		}
		j.SHA256 = cp.SHA256
		if err := m.set(paneldb.PhaseCopied); err != nil {
			return err
		}
		if err := m.crash("copied"); err != nil {
			return err
		}
	}
	if j.Phase == paneldb.PhaseCopied {
		// re-verify the copy and its marker before the source goes
		if err := m.verifyDisk(ctx, folderDB, "", j.SHA256); err != nil {
			return err
		}
		if err := m.set(paneldb.PhaseMounting); err != nil { // before the rename: the source may be gone from db/
			return err
		}
	}
	if j.Phase == paneldb.PhaseMounting {
		p, err := m.op(ctx, "probe", "", none, none)
		if err != nil {
			return err
		}
		if !p.Mounted {
			for _, r := range []struct {
				from, to hostdisk.DBFile
				opt      bool
			}{{dbMain, moving, false}, {file(hostdisk.AreaDB, paneldb.FileName+"-wal"), movingWal, true}} {
				if _, err := m.op(ctx, "rename", "no", r.from, r.to, r.opt); err != nil {
					return err
				}
				if err := m.crash("renamed-" + r.to.Name); err != nil {
					return err
				}
			}
			if _, err := m.op(ctx, "rm", "no", file(hostdisk.AreaDB, paneldb.FileName+"-shm"), none, true); err != nil {
				return err
			}
			if _, err := m.op(ctx, "mount", "", none, none); err != nil {
				return err
			}
			if err := m.crash("mount-unit-started"); err != nil {
				return err
			}
		}
		if err := m.verifyDisk(ctx, dbMain, "yes", j.SHA256); err != nil {
			return err
		}
		if err := m.set(paneldb.PhaseMounted); err != nil {
			return err
		}
		if err := m.crash("mounted"); err != nil {
			return err
		}
	}
	if j.Phase == paneldb.PhaseMounted {
		return m.startNew(ctx)
	}
	return fmt.Errorf("move.journal phase %q is not a move-in stage", j.Phase)
}

// verifyDisk: the file's hash is the journal's, and (folder or mounted db/)
// the marker is this install's.
func (m *mover) verifyDisk(ctx context.Context, f hostdisk.DBFile, mounted, sha string) error {
	h, err := m.op(ctx, "hash", mounted, f, none)
	if err != nil {
		return err
	}
	if !h.Exists || h.SHA256 != sha {
		return fmt.Errorf("%s does not match the journal's hash", f.Name)
	}
	if f.Area == hostdisk.AreaSwap || (f.Area == hostdisk.AreaDB && mounted == "no") {
		return nil
	}
	area := f.Area
	mk, err := m.op(ctx, "marker-read", mounted, file(area, ""), none)
	if err != nil {
		return err
	}
	if mk.Marker == nil || mk.Marker.InstallID != m.j.InstallID || mk.Marker.DiskID != m.j.DiskID {
		return errors.New("the disk folder has no marker of this install")
	}
	return nil
}

func (m *mover) moveBack(ctx context.Context) error {
	j := &m.j
	incoming := file(hostdisk.AreaSwap, incomingName)
	if j.Phase == paneldb.PhasePrepared {
		if err := m.stopOld(ctx); err != nil {
			return err
		}
		if err := m.crash("stopped"); err != nil {
			return err
		}
		if _, err := m.op(ctx, "checkpoint", "yes", dbMain, none); err != nil {
			return err
		}
		cp, err := m.op(ctx, "copy", "yes", dbMain, incoming)
		if err != nil {
			return err
		}
		src, err := m.op(ctx, "hash", "yes", dbMain, none)
		if err != nil {
			return err
		}
		if !src.Exists || src.SHA256 != cp.SHA256 {
			return fmt.Errorf("the copy's hash %s is not the source's %s", cp.SHA256, src.SHA256)
		}
		if _, err := m.op(ctx, "integrity", "", incoming, none); err != nil {
			return err
		}
		j.SHA256 = cp.SHA256
		if err := m.set(paneldb.PhaseCopied); err != nil {
			return err
		}
		if err := m.crash("copied"); err != nil {
			return err
		}
	}
	if j.Phase == paneldb.PhaseCopied {
		if err := m.verifyDisk(ctx, incoming, "", j.SHA256); err != nil {
			return err
		}
		if err := m.set(paneldb.PhaseUnmounting); err != nil {
			return err
		}
	}
	if j.Phase == paneldb.PhaseUnmounting {
		if _, err := m.op(ctx, "unmount", "", none, none); err != nil {
			return err
		}
		if err := m.crash("unit-removed"); err != nil {
			return err
		}
		if err := m.set(paneldb.PhaseUnmounted); err != nil {
			return err
		}
	}
	if j.Phase == paneldb.PhaseUnmounted {
		if _, err := m.op(ctx, "unlock", "no", none, none); err != nil {
			return err
		}
		if _, err := m.op(ctx, "rename", "no", incoming, dbMain); err != nil {
			return err
		}
		if err := m.crash("incoming-renamed"); err != nil {
			return err
		}
		if err := m.set(paneldb.PhasePlaced); err != nil {
			return err
		}
	}
	if j.Phase == paneldb.PhasePlaced {
		if err := m.verifyDisk(ctx, dbMain, "no", j.SHA256); err != nil {
			return err
		}
		return m.startNew(ctx)
	}
	return fmt.Errorf("move.journal phase %q is not a move-back stage", j.Phase)
}

// startNew journals starting-new, runs the new panel and gates it.
func (m *mover) startNew(ctx context.Context) error {
	if err := m.set(paneldb.PhaseStartingNew); err != nil {
		return err
	}
	if err := m.crash("starting-new"); err != nil {
		return err
	}
	id, err := m.l.docker.Run(ctx, m.spec)
	if err != nil {
		return fmt.Errorf("run the new panel: %w", err)
	}
	if err := m.set(paneldb.PhaseGating); err != nil {
		return err
	}
	if err := m.crash("gating"); err != nil {
		return err
	}
	if err := m.l.gate(ctx, id, m.spec); err != nil {
		m.l.recordFailure(ctx, id, paneldb.Path(m.dataDir), paneldb.Path(m.dataDir), m.spec.Image[strings.LastIndex(m.spec.Image, ":")+1:], err)
		return fmt.Errorf("new panel: %w", err)
	}
	return m.pass(ctx)
}

// regate is the resume from starting-new or gating: the recorded container, if
// it exists, is gated again and kept when it passes, never rolled back for slow
// health. A missing container with no gate result rolls back.
func (m *mover) regate(ctx context.Context) error {
	all, _, err := m.panels(ctx)
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.Name != m.j.Container {
			continue
		}
		if d, err := m.l.docker.Inspect(ctx, c.ID); err == nil && d.Running && m.l.gate(ctx, c.ID, m.spec) == nil {
			return m.pass(ctx)
		}
	}
	return m.rollback(ctx, errors.New("the new panel is not there or did not pass its gate"))
}

// pass finishes a move whose new panel is healthy. Every step can be repeated.
func (m *mover) pass(ctx context.Context) error {
	j := &m.j
	if j.Direction == paneldb.DirIn {
		// the old copy: the source files become the "Old copy" the card offers
		for _, suf := range []string{"", "-wal"} {
			if _, err := m.op(ctx, "rename", "", file(hostdisk.AreaSwap, movingName+suf), file(hostdisk.AreaSwap, j.OldCopy+suf), true); err != nil {
				return err
			}
			if err := m.crash("old-copy-renamed" + suf); err != nil {
				return err
			}
		}
	}
	if err := m.removePredecessors(ctx); err != nil {
		return err
	}
	if j.Direction == paneldb.DirBack {
		// the disk copy becomes the old copy on the disk; the marker goes first
		if _, err := m.op(ctx, "rm", "", file(hostdisk.AreaFolder, hostdisk.PanelDBMarker), none); err != nil {
			return err
		}
		if _, err := m.op(ctx, "rename", "", file(hostdisk.AreaFolder, paneldb.FileName), file(hostdisk.AreaFolder, j.OldCopy)); err != nil {
			return err
		}
		if err := m.crash("disk-copy-renamed"); err != nil {
			return err
		}
		return paneldb.WriteJournal(m.dataDir, paneldb.Journal{Phase: paneldb.PhaseNone, Direction: paneldb.DirBack, MoveID: j.MoveID, OldCopy: j.OldCopy})
	}
	moved := paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, MoveID: j.MoveID, DiskID: j.DiskID, InstallID: j.InstallID, Folder: j.Folder, OldCopy: j.OldCopy}
	return paneldb.WriteJournal(m.dataDir, moved)
}

func (m *mover) removePredecessors(ctx context.Context) error {
	_, pred, err := m.panels(ctx)
	if err != nil {
		return err
	}
	for _, c := range pred {
		if err := m.l.docker.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove the old panel %s: %w", c.Name, err)
		}
	}
	return nil
}

// rollback undoes the move with the journal at rolling-back. Every step is
// safe to repeat; a failure returns with the journal still rolling-back, so
// the next run resumes this rollback. Nothing here deletes a database file
// whose other copy has not been checked against the journal's hash.
func (m *mover) rollback(ctx context.Context, cause error) error {
	slog.Warn("panel database move: rolling back", "direction", m.j.Direction, "from_phase", m.j.Phase, "err", cause)
	if m.j.Phase != paneldb.PhaseRollingBack {
		if err := m.set(paneldb.PhaseRollingBack); err != nil {
			return errors.Join(cause, err)
		}
	}
	// the new panel goes first: nothing may write while the files move
	all, pred, err := m.panels(ctx)
	if err != nil {
		return errors.Join(cause, err)
	}
	for _, c := range all {
		if c.Name == m.j.Container && !slices.ContainsFunc(pred, func(p docker.Container) bool { return p.ID == c.ID }) {
			if err := m.l.docker.StopRemove(ctx, c.ID); err != nil {
				return errors.Join(cause, fmt.Errorf("remove the new panel: %w", err))
			}
		}
	}
	var rerr error
	if m.j.Direction == paneldb.DirIn {
		rerr = m.rollbackIn(ctx)
	} else {
		rerr = m.rollbackBack(ctx)
	}
	if rerr != nil {
		return errors.Join(cause, fmt.Errorf("roll back (the journal stays; run stackr-install on this node): %w", rerr))
	}
	if err := m.crash("rolled-back-files"); err != nil {
		return err
	}
	for _, c := range pred {
		if err := m.l.docker.Start(ctx, c.ID); err != nil {
			return errors.Join(cause, fmt.Errorf("start the old panel: %w", err))
		}
	}
	var jerr error
	if m.j.Direction == paneldb.DirIn {
		jerr = paneldb.ClearJournal(m.dataDir)
	} else {
		jerr = paneldb.WriteJournal(m.dataDir, m.j.MovedBase())
	}
	if jerr != nil {
		return errors.Join(cause, jerr)
	}
	return fmt.Errorf("%w: %w", ErrRolledBack, cause)
}

// rollbackIn puts a move-in back: the unit goes, the files come back from
// swap/, the disk's copy and marker go (best effort: the disk may be the
// thing that failed, and the next move wipes them anyway).
func (m *mover) rollbackIn(ctx context.Context) error {
	if _, err := m.op(ctx, "unmount", "", none, none); err != nil {
		return err
	}
	if err := m.crash("rb-unmounted"); err != nil {
		return err
	}
	if _, err := m.op(ctx, "unlock", "no", none, none); err != nil {
		return err
	}
	if err := m.crash("rb-unlocked"); err != nil {
		return err
	}
	// .moving back to db/stackr.db. Gone with the database in db/ is done; both
	// there is refused (never guessed), and neither is a loss that is reported.
	if _, err := m.op(ctx, "rename", "no", file(hostdisk.AreaSwap, movingName), dbMain); err != nil {
		return err
	}
	if err := m.crash("rb-renamed"); err != nil {
		return err
	}
	if _, err := m.op(ctx, "rename", "no", file(hostdisk.AreaSwap, movingName+"-wal"), file(hostdisk.AreaDB, paneldb.FileName+"-wal"), true); err != nil {
		return err
	}
	for _, f := range []string{paneldb.FileName, paneldb.FileName + ".partial", hostdisk.PanelDBMarker} {
		if _, err := m.op(ctx, "rm", "", file(hostdisk.AreaFolder, f), none); err != nil {
			slog.Warn("panel database move: disk folder not cleaned (the next move wipes it)", "file", f, "err", err)
		}
	}
	return nil
}

// rollbackBack puts a move-back back: the database goes on the disk copy,
// which the journal's hash proves is whole, and the mount returns.
func (m *mover) rollbackBack(ctx context.Context) error {
	folderDB := file(hostdisk.AreaFolder, paneldb.FileName)
	if m.j.SHA256 != "" {
		// the disk copy is the source of a move-back and was never touched
		if err := m.verifyDisk(ctx, folderDB, "", m.j.SHA256); err != nil {
			return fmt.Errorf("the disk copy cannot be verified: %w", err)
		}
	}
	p, err := m.op(ctx, "probe", "", none, none)
	if err != nil {
		return err
	}
	if !p.Mounted {
		if m.j.SHA256 == "" {
			return errors.New("not mounted and no verified disk copy: refusing to touch db/")
		}
		if _, err := m.op(ctx, "unlock", "no", none, none); err != nil {
			return err
		}
		if _, err := m.op(ctx, "clear-underlay", "no", none, none); err != nil {
			return err
		}
		if err := m.crash("rb-cleared"); err != nil {
			return err
		}
		if _, err := m.op(ctx, "mount", "", none, none); err != nil {
			return err
		}
	}
	if _, err := m.op(ctx, "rm", "", file(hostdisk.AreaSwap, incomingName), none); err != nil {
		return err
	}
	if m.j.SHA256 == "" {
		return nil
	}
	return m.verifyDisk(ctx, dbMain, "yes", m.j.SHA256)
}
