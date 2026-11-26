package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/paneldb"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Move the panel database onto the controller's node disk, and back. The panel
// cannot move its own database, so the job does the planning, the archive and
// the folder, then launches the helper (leaf/panel move.go) that stops the
// panel, copies, mounts and starts it again. The state is move.journal.

const kindMovePanelDB = "move-paneldb"

// movePanelKey is held by a move: a second press is refused while one is live.
const movePanelKey = "paneldb-move"

type movePanelJob struct {
	NodeID string `json:"node_id"`
	Back   bool   `json:"back"`
	MoveID string `json:"move_id"`
	By     string `json:"by,omitempty"`
}

// The warning the plan dialog shows for a move onto the disk.
const panelDBWarning = "After this, the whole control plane depends on this one disk. If the disk is missing the panel does not start, and nothing alerts you while it is down. To bring the disk back without a reboot, run stackr-install disk-check on the node."

// PanelDBPlan is what the move dialog shows before the confirm.
type PanelDBPlan struct {
	NodeID     string
	DiskID     string
	Back       bool
	SizeBytes  int64  // the database and its -wal now
	DiskFree   uint64 // free on the node disk
	SystemFree uint64 // free under the data dir
	Need       int64  // the database plus a tenth, on each disk the move writes to
	Fits       bool
	Restarts   string // what restarts
	Warning    string // the single-point-of-failure text (a move onto the disk)
	Refusal    string // "" = the move may start
}

// PanelDBView is the Node disk card's state for the panel database row.
type PanelDBView struct {
	Applicable    bool   // controller, installed panel, a ready whole disk
	Reason        string // why not, when it is not
	OnDisk        bool
	Moving        bool // a move is queued or running, or the journal says one is unfinished
	DiskID        string
	OldCopy       bool      // an old copy waits for Remove old copy
	OldCopyOnDisk bool      // it is on the node disk (after a move back), else on the system disk
	OldCopyAt     time.Time // when it was taken (the card shows DD-MM)
}

func (o *Orchestrator) panelJournal() (paneldb.Journal, error) {
	return paneldb.ReadJournal(o.cfg.DataDir)
}

// panelDBDiskRefusal is Forget's and Adopt's refusal while the panel database
// is, or is being moved, on this disk.
func (o *Orchestrator) panelDBDiskRefusal(d store.NodeDisk) error {
	j, err := o.panelJournal()
	if err != nil {
		return err
	}
	// an Update running on a moved install (Moved) is still the database on the disk
	inMove := j.Phase != paneldb.PhaseNone && j.Phase != paneldb.PhaseFresh && j.Direction != paneldb.DirLayout && j.Direction != paneldb.DirUpdate && j.Direction != paneldb.DirRestore
	if (j.Moved || inMove) && (j.DiskID == d.ID || j.DiskID == "") {
		return errs.Refusedf("The panel database is on this disk. Move it back to the system disk first.")
	}
	return nil
}

// movePanelLive: a move job is queued or running.
func (o *Orchestrator) movePanelLive(ctx context.Context) bool {
	live, err := o.jobRows.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return true
	}
	for _, j := range live {
		if j.Kind == kindMovePanelDB {
			return true
		}
	}
	return false
}

// movePanelLanded: a move job still running when the helper stopped the old
// panel, whose move the journal says went through. It ends done.
func (o *Orchestrator) movePanelLanded(j store.Job) bool {
	if j.Kind != kindMovePanelDB {
		return false
	}
	var p movePanelJob
	if json.Unmarshal([]byte(j.Payload), &p) != nil {
		return false
	}
	jr, err := o.panelJournal()
	if err != nil || jr.MoveID != p.MoveID {
		return false
	}
	// the panel booting is the new one: its journal is at starting-new or gating
	// of this move, or already at the end
	trial := jr.Phase == paneldb.PhaseStartingNew || jr.Phase == paneldb.PhaseGating
	if p.Back {
		return jr.Direction == paneldb.DirBack && (trial || jr.Phase == paneldb.PhaseNone)
	}
	return jr.Direction == paneldb.DirIn && (trial || jr.Phase == paneldb.PhaseMoved)
}

// PanelDBState reads the card's row. Reads only: it makes no helper run.
func (o *Orchestrator) PanelDBState(ctx context.Context, nodeID string) (PanelDBView, error) {
	var v PanelDBView
	if reason := o.panelDBApplicable(ctx, nodeID); reason != "" {
		v.Reason = reason
		return v, nil
	}
	d, _, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return v, err
	}
	j, err := o.panelJournal()
	if err != nil {
		return v, err
	}
	v.Applicable, v.DiskID = true, d.ID
	v.OnDisk = j.Phase == paneldb.PhaseMoved
	v.Moving = o.movePanelLive(ctx) || (j.Phase != paneldb.PhaseNone && j.Phase != paneldb.PhaseMoved && j.Phase != paneldb.PhaseFresh &&
		(j.Direction == paneldb.DirIn || j.Direction == paneldb.DirBack))
	if j.OldCopy != "" && (j.Phase == paneldb.PhaseMoved || (j.Phase == paneldb.PhaseNone && j.Direction == paneldb.DirBack)) {
		v.OldCopy = true
		v.OldCopyOnDisk = j.Direction == paneldb.DirBack
		if _, err := os.Stat(filepath.Join(paneldb.Swap(o.cfg.DataDir), j.OldCopy)); v.OldCopyOnDisk || err == nil {
			v.OldCopyAt = oldCopyTime(j.OldCopy)
		} else {
			v.OldCopy = false // the file is gone: nothing to remove
		}
	}
	return v, nil
}

// oldCopyTime reads the UTC timestamp out of stackr.db.moved-<stamp>-<id>.
func oldCopyTime(name string) time.Time {
	_, rest, ok := strings.Cut(name, ".moved-")
	if !ok {
		return time.Time{}
	}
	stamp, _, _ := strings.Cut(rest, "-")
	t, _ := time.Parse(paneldb.Stamp, stamp)
	return t
}

// panelDBApplicable is "" when the node may hold the move, else the reason it
// may not (the row is then hidden).
func (o *Orchestrator) panelDBApplicable(ctx context.Context, nodeID string) string {
	ctl, err := o.controllerID(ctx)
	switch {
	case err != nil || ctl != nodeID:
		return "Only the controller holds the panel database."
	case !o.cfg.installedPanel():
		return "This panel does not run as an installed container."
	}
	d, has, err := o.nodeDisk(ctx, nodeID)
	switch {
	case err != nil || !has || d.State != disk.StateReady:
		return "This node's disk is not ready."
	case d.Kind != disk.KindXFS:
		return "Only a whole disk can hold the panel database."
	}
	return ""
}

func (o *Orchestrator) panelProbe(ctx context.Context, nodeID, diskID string) (*hostdisk.DBResult, *hostdisk.Error, error) {
	res, err := o.dh.Disk(ctx, nodeID, hostdisk.Request{Op: "probe", Kind: "paneldb", DB: &hostdisk.DBReq{DataDir: o.cfg.DataDir, DiskID: diskID}})
	if err != nil {
		return nil, nil, err
	}
	return res.DB, res.Error, nil
}

// PlanMovePanelDB is read only, but it does run the disk helper for the host
// facts (symlinks, chattr, free space). Advisory: the job judges again.
func (o *Orchestrator) PlanMovePanelDB(ctx context.Context, by *Principal, nodeID string, back bool) (PanelDBPlan, error) {
	return o.planWith(ctx, nodeID, back, false)
}

// planWith is the plan; own = the caller is the move job, which holds the
// move key itself.
func (o *Orchestrator) planWith(ctx context.Context, nodeID string, back, own bool) (PanelDBPlan, error) {
	p := PanelDBPlan{NodeID: nodeID, Back: back, Restarts: "The panel restarts. Proxy, registry, tiles and logs keep running."}
	if !back {
		p.Warning = panelDBWarning
	}
	if reason := o.panelDBApplicable(ctx, nodeID); reason != "" {
		p.Refusal = reason
		return p, nil
	}
	d, _, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return p, err
	}
	p.DiskID = d.ID
	j, err := o.panelJournal()
	if err != nil {
		return p, err
	}
	switch {
	case j.Phase == paneldb.PhaseMoved && !back:
		p.Refusal = "The panel database is already on this disk."
	case j.Phase != paneldb.PhaseMoved && back && j.Phase == paneldb.PhaseNone:
		p.Refusal = "The panel database is on the system disk."
	case j.Phase != paneldb.PhaseNone && j.Phase != paneldb.PhaseMoved:
		p.Refusal = "A move is unfinished or was interrupted. Run stackr-install on this node: it reads move.journal and finishes or rolls back."
	case back && j.OldCopy != "" && fileExists(filepath.Join(paneldb.Swap(o.cfg.DataDir), j.OldCopy)):
		p.Refusal = "Remove the old copy of the database on the system disk first."
	case o.UpgradeUnderway(ctx):
		p.Refusal = "An update is running."
	case !own && o.movePanelLive(ctx):
		p.Refusal = "The panel database is already moving."
	}
	// the job's own lock set holds todiskKey, so it would refuse itself
	if p.Refusal == "" && !own {
		if live, err := o.todiskLive(ctx, nodeID); err != nil {
			return p, err
		} else if live {
			p.Refusal = "Volumes are moving to the disk. Wait for them."
		}
	}
	if p.Refusal != "" {
		return p, nil
	}
	if back {
		// a disk that is not there cannot be moved back from: nothing is changed
		if _, _, err := o.dataDiskOf(ctx, nodeID); err != nil {
			p.Refusal = "The disk is not there. Bring it back first (stackr-install disk-check on the node)."
			return p, nil
		}
	}
	db, herr, err := o.panelProbe(ctx, nodeID, d.ID)
	if err != nil {
		return p, err
	}
	switch {
	case herr != nil && herr.Code == hostdisk.CodeSymlink:
		p.Refusal = "The data folder path goes through a symbolic link. The move needs the real path."
	case herr != nil:
		return p, herr
	case !db.ChattrOK:
		p.Refusal = "The system disk's filesystem does not support chattr +i, which protects the empty folder under the mount."
	}
	if p.Refusal != "" {
		return p, nil
	}
	p.SizeBytes, p.SystemFree = db.DBBytes, db.SysFree
	p.Need = p.SizeBytes + p.SizeBytes/10
	if p.DiskFree, _, err = o.dataDiskOf(ctx, nodeID); err != nil {
		p.Refusal = "The disk is not there."
		return p, nil
	}
	// the archive and the kept copy land on the system disk; the copy on the disk
	if back {
		p.Fits = uint64(p.Need) <= p.SystemFree
	} else {
		p.Fits = uint64(p.Need) <= p.DiskFree && uint64(2*p.Need) <= p.SystemFree
	}
	if !p.Fits {
		p.Refusal = fmt.Sprintf("Not enough room: the database is %s. The disk has %s free and the system disk %s.", gb(uint64(p.SizeBytes)), gb(p.DiskFree), gb(p.SystemFree))
	}
	return p, nil
}

// MovePanelDB queues the move (node.manage). A second press is refused.
func (o *Orchestrator) MovePanelDB(ctx context.Context, by *Principal, nodeID string, back bool) (Job, error) {
	if err := o.erroringDiskGate(ctx, nodeID); err != nil {
		return Job{}, err
	}
	p, err := o.PlanMovePanelDB(ctx, by, nodeID, back)
	if err != nil {
		return Job{}, err
	}
	if p.Refusal != "" {
		return Job{}, errs.Refusedf("%s", p.Refusal)
	}
	pj := movePanelJob{NodeID: nodeID, Back: back, MoveID: uuid.NewString()}
	if by != nil {
		pj.By = by.User.ID
	}
	j, err := o.enqueueExclusive(ctx, kindMovePanelDB, pj, []string{movePanelKey}, errs.Conflictf("The panel database is already moving."),
		job.All, movePanelKey, todiskKey(nodeID))
	if err == nil {
		o.auditDiskLog(ctx, by, "disk.move_panel_db", nodeID, map[bool]string{false: "to the disk", true: "back"}[back])
	}
	return j, err
}

// runMovePanelDB is the job: judge again, place the folder, archive and launch
// the helper. The old panel dies soon after the helper starts; the row ends
// done through Survived once the journal shows the move went through.
func (o *Orchestrator) runMovePanelDB(ctx context.Context, r *jobs.Run, p movePanelJob) error {
	ctx, err := o.withController(ctx)
	if err != nil {
		return err
	}
	// this job holds the move key itself: only the other refusals count
	plan, err := o.planForJob(ctx, p)
	if err != nil {
		return err
	}
	if plan.Refusal != "" {
		return errs.Refusedf("%s", plan.Refusal)
	}
	d, _, err := o.nodeDisk(ctx, p.NodeID)
	if err != nil {
		return err
	}
	if !p.Back {
		if _, err := o.diskLeaf.Place(ctx, d, hostdisk.PanelDBFolder); err != nil {
			return fmt.Errorf("place the folder: %w", err)
		}
	}
	image, err := o.runningPanelImage(ctx)
	if err != nil {
		return err
	}
	mv := panel.Move{Dir: paneldb.DirIn, MoveID: p.MoveID, DiskID: d.ID, InstallID: o.cfg.InstallID,
		Folder: hostdisk.MountPoint(d.ID) + "/" + d.ID + "/" + hostdisk.PanelDBFolder, Stamp: time.Now().UTC().Format(paneldb.Stamp)}
	if p.Back {
		mv.Dir = paneldb.DirBack
	}
	archive, err := o.upgrade.Move(ctx, image, mv, r.Log)
	if err != nil {
		o.upgradeStep.Store(0)
		return err
	}
	o.holdUpgrade()
	_, _ = fmt.Fprintf(r.Log, "pre-move archive %s; the helper moves the database now (%s)\n", archive, mv.Dir)
	return nil
}

// planForJob is the plan without the "already moving" refusal of the job's own key.
func (o *Orchestrator) planForJob(ctx context.Context, p movePanelJob) (PanelDBPlan, error) {
	plan, err := o.planWith(ctx, p.NodeID, p.Back, true)
	return plan, err
}

// RemovePanelDBOldCopy is the only deleter of the old copy (node.manage).
func (o *Orchestrator) RemovePanelDBOldCopy(ctx context.Context, by *Principal, nodeID string) error {
	if reason := o.panelDBApplicable(ctx, nodeID); reason != "" {
		return errs.Refusedf("%s", reason)
	}
	if o.movePanelLive(ctx) || o.UpgradeUnderway(ctx) {
		return errs.Conflictf("The panel database is moving or updating. Try again when it is done.")
	}
	j, err := o.panelJournal()
	if err != nil {
		return err
	}
	if j.OldCopy == "" || (j.Phase != paneldb.PhaseMoved && !(j.Phase == paneldb.PhaseNone && j.Direction == paneldb.DirBack)) {
		return errs.Refusedf("There is no old copy to remove.")
	}
	name := j.OldCopy
	if j.Phase == paneldb.PhaseMoved { // on the system disk, in swap/
		for _, suf := range []string{"", "-wal"} {
			if err := os.Remove(filepath.Join(paneldb.Swap(o.cfg.DataDir), name+suf)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		j.OldCopy = ""
		err = paneldb.WriteJournal(o.cfg.DataDir, j.MovedBase())
	} else { // on the node disk: the folder and its row go with it
		d, _, derr := o.nodeDisk(ctx, nodeID)
		if derr != nil {
			return derr
		}
		res, derr := o.dh.Disk(ctx, nodeID, hostdisk.Request{Op: "rm", Kind: "paneldb",
			DB: &hostdisk.DBReq{DataDir: o.cfg.DataDir, DiskID: d.ID, From: hostdisk.DBFile{Area: hostdisk.AreaFolder, Name: name}}})
		if derr != nil {
			return derr
		}
		if res.Error != nil {
			return res.Error
		}
		if derr := o.diskLeaf.Drop(ctx, d, hostdisk.PanelDBFolder); derr != nil {
			return derr
		}
		err = paneldb.ClearJournal(o.cfg.DataDir)
	}
	if err == nil {
		o.auditDiskLog(ctx, by, "disk.remove_panel_db_copy", nodeID, name)
	}
	return err
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
