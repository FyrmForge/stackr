package admin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The node drawer's Node disk section and the Add disk dialog's three POSTs
// (probe, state, add). Check disk is nodeJob in mountNodes.

// diskRows is how many volumes the disk section lists before "Show all".
const diskRows = 8

// nodeDisk is the node drawer's Node disk section.
func (h *handler) nodeDisk(c echo.Context, d service.NodeDetail, base string, live service.NodeLive) (ui.NodeDisk, error) {
	p, err := h.orch.NodeDiskPanel(c.Request().Context(), d.ID)
	if err != nil {
		return ui.NodeDisk{}, err
	}
	dk := ui.NodeDisk{
		Has: p.Has, State: p.State, Path: p.Path, Network: p.Network, Stale: p.Stale, Count: len(p.Volumes),
		Dialog: ui.DiskDialog{Probe: base + "/disk/probe", State: base + "/disk/state", Add: base + "/disk/add", Choose: base + "/disk/choose", Pick: base + "/disk/pick", Plan: base + "/disk/format/plan", Node: d.Name},
	}
	online := d.Status == "online"
	if !p.Has {
		dk.Help = "Keep volumes on their own disk, apart from the system disk."
		adding, err := h.jobActive(c, d.ID, kindsAdd...)
		if err != nil {
			return dk, err
		}
		adopting, err := h.jobActive(c, d.ID, "disk-adopt")
		if err != nil {
			return dk, err
		}
		switch {
		case adding:
			dk.Why = "Adding the disk."
		case adopting: // queued: the row says adopting once the job records it
			dk.Why = "Adopting this disk."
		case p.Refusal != "":
			dk.Help = p.Refusal
		case !online:
			dk.Why = "The node is offline."
		case !d.Controller && (live.Facts == nil || !live.Facts.DisksRead):
			dk.Help = "Update this node to add a disk."
		default:
			dk.Add = base + "/disk/add"
			dk.Adopt = &ui.DiskAdopt{Plan: base + "/disk/adopt/plan"}
			dk.Help += " Or adopt a disk from a node that is gone."
		}
		return dk, nil
	}
	dk.ID = p.ID
	if p.Kind == "xfs" {
		if err := h.xfsDisk(c, &dk, p, d, base, online); err != nil {
			return dk, err
		}
	}
	if p.State == "ready" {
		dk.Help = "New volumes on this node are stored here."
		if p.Total > 0 && p.Damaged == "" {
			dk.Used = diskSize(int64(p.Total-p.Free)) + " of " + diskSize(int64(p.Total))
			dk.StaleAt = render.Ago(p.ReadAt)
			dk.Pct = strconv.Itoa(int((p.Total - p.Free) * 100 / p.Total))
			dk.Low = p.Low
		}
	}
	if !p.MissingSince.IsZero() {
		dk.Since = ago(p.MissingSince)
	}
	if (p.State != "ready" || p.Damaged != "") && online {
		dk.Check = base + "/disk/check"
		if dk.Whole && p.State == "adding" && !p.Formatted {
			dk.Check = "" // nothing was formatted yet: there is nothing to check
		}
		if dk.Checking, err = h.jobActive(c, d.ID, "disk-check"); err != nil {
			return dk, err
		}
	}
	if p.State == "adding" && online {
		adding, err := h.jobActive(c, d.ID, kindsAdd...)
		if err != nil {
			return dk, err
		}
		if dk.AddFailed = !adding; dk.AddFailed {
			if dk.Whole { // try again picks the disk again; a formatted one is only mounted
				dk.Retry = base + "/disk/choose"
			} else {
				dk.Add = base + "/disk/add"
				dk.Dialog.Path = p.Path
			}
		}
	}
	if err := h.diskActions(c, &dk, p, d, base, online); err != nil {
		return dk, err
	}
	for i, r := range p.Volumes {
		row := ui.DiskVolRow{Name: r.Slug, Where: r.Where, Size: "unknown", Orphaned: r.Orphaned}
		if r.Open != "" {
			row.Open = r.Open + "/-/volumes/" + r.ID
		}
		if r.SizeBytes >= 0 && p.State == "ready" {
			row.Size = diskSize(r.SizeBytes)
		}
		if i < diskRows {
			dk.Rows = append(dk.Rows, row)
		} else {
			dk.More = append(dk.More, row)
		}
	}
	return dk, nil
}

// kindsAdd are the jobs that add the node's disk: a folder, or a whole disk.
var kindsAdd = []string{"disk-add", "disk-add-device"}

// jobActive is whether a job of one of kinds for the node is queued or running.
func (h *handler) jobActive(c echo.Context, nodeID string, kinds ...string) (bool, error) {
	js, err := h.orch.Jobs(c.Request().Context(), 0, "queued", "waiting", "running")
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(js, func(j service.Job) bool {
		var p struct {
			NodeID string `json:"node_id"`
		}
		return slices.Contains(kinds, j.Kind) && json.Unmarshal([]byte(j.Payload), &p) == nil && p.NodeID == nodeID
	}), nil
}

// diskSize is a disk size as the section says it: 86 MB, 4.2 GB, 278 GB, 1.8 TB.
func diskSize(b int64) string { return render.DiskSize(b) }

// diskDialog is the dialog's fields as posted.
func diskDialog(c echo.Context) ui.DiskDialog {
	base := nodesBase + "/" + c.Param("node")
	return ui.DiskDialog{
		Probe: base + "/disk/probe", State: base + "/disk/state", Add: base + "/disk/add",
		Choose: base + "/disk/choose", Pick: base + "/disk/pick", Plan: base + "/disk/format/plan",
		Path:    strings.TrimSpace(c.FormValue("path")),
		Checked: c.FormValue("checked"), Network: c.FormValue("net") == "1", NetOK: c.FormValue("net_ok") != "",
	}
}

// probeText is the words for a refused probe, by the helper's code.
func probeText(code, msg string) string {
	switch code {
	case hostdisk.CodeRootFS:
		return "This folder is on the system disk. Use a separate disk."
	case hostdisk.CodeNotMountPoint:
		if strings.HasPrefix(msg, "Use the real path") {
			return msg // canonical's: the folder is a symlink, the message names the real path
		}
		return "Not a mount point. Mount the disk there first."
	case hostdisk.CodeDiskMissing:
		return "No such folder."
	case hostdisk.CodeReadOnly:
		return "Read-only. Mount it writable."
	case hostdisk.CodeNotEmpty:
		return "Not empty. Use an empty folder."
	case hostdisk.CodeForeignStamp:
		return "Another stackr disk is already here."
	case hostdisk.CodeBadName:
		return "Start the path with /."
	}
	return msg
}

// POST …/nodes/:node/disk/probe: Check. The helper runs here and nowhere
// else; the answer is the dialog's result, always 200.
func (h *handler) diskProbe(c echo.Context) error {
	ctx := c.Request().Context()
	d := diskDialog(c)
	d.Input, d.Tone, d.Invalid = true, "err", true
	d.Checked, d.Network, d.NetOK = "", false, false // a failed probe passes nothing
	switch {
	case d.Path == "":
		d.Msg = "Give a full path to a mounted folder, like /mnt/data."
	case !strings.HasPrefix(d.Path, "/"):
		d.Msg = "Start the path with /."
	default:
		p, err := h.orch.ProbeNodeFolder(ctx, middleware.Principal(c), c.Param("node"), d.Path)
		var down *service.NodeDownError
		onField, top, refused := render.OnField(err, "path")
		var he *hostdisk.Error
		switch {
		case err != nil && !errors.As(err, &down) && errors.As(err, &he) && he.Code == hostdisk.CodeIO:
			d.Msg = he.Message
		case err != nil && (errors.As(err, &down) || service.DockerUnreachable(err) || errors.Is(err, context.DeadlineExceeded)):
			name := c.Param("node")
			if n, nerr := h.orch.NodeDetail(ctx, name); nerr == nil {
				name = n.Name
			}
			d.Msg = name + " did not answer. Try again."
		case err != nil && refused:
			d.Msg = cmp.Or(onField, top)
		case err != nil:
			return middleware.HTTPError(err) // not the node's silence: show the real cause
		case p.Code != "":
			d.Msg = probeText(p.Code, p.Message)
		default:
			d.Msg, d.Tone, d.Invalid = "Mounted, empty, "+diskSize(int64(p.Free))+" free.", "ok", false
			if p.Network {
				d.Msg += " Network disk (" + p.FSType + ")."
			}
			d.Checked, d.Network = d.Path, p.Network
		}
	}
	return respond.HTML(c, http.StatusOK, ui.DiskState(d))
}

// POST …/nodes/:node/disk/state: the path or the network box changed. No
// helper runs: an edited path drops the answer, a ticked box only moves
// Use folder.
func (h *handler) diskState(c echo.Context) error {
	d := diskDialog(c)
	if d.Checked != "" && d.Path == d.Checked {
		d.Tone = "ok"
		return respond.HTML(c, http.StatusOK, ui.DiskUse(d))
	}
	d.Checked, d.Network, d.NetOK = "", false, false
	return respond.HTML(c, http.StatusOK, ui.DiskState(d))
}

// POST …/nodes/:node/disk/add: Use folder. Only the path Check passed is
// added; a refusal lands in the dialog, a queued add answers the drawer.
func (h *handler) diskAdd(c echo.Context) error {
	d := diskDialog(c)
	fail := func(msg string) error {
		d.Msg, d.Tone, d.Invalid, d.Input = msg, "err", true, true
		d.Checked, d.Network, d.NetOK = "", false, false
		c.Response().Header().Set("HX-Reswap", "none")
		return respond.HTML(c, http.StatusOK, ui.DiskState(d))
	}
	switch {
	case d.Checked == "" || d.Path != d.Checked:
		return fail("Check the folder first.")
	case d.Network && !d.NetOK:
		return fail("Tick the box to use a network disk.")
	}
	j, err := h.orch.AddNodeFolder(c.Request().Context(), middleware.Principal(c), c.Param("node"), d.Path)
	if err != nil {
		if onField, top, ok := render.OnField(err, "path"); ok {
			return fail(cmp.Or(onField, top))
		}
		return middleware.HTTPError(err)
	}
	c.Set("job", j)
	return h.nodeAnswer(c, "overview", "Adding the disk.", nil)
}

// diskActions fills the forget, resume and move-all parts of the section. Rows
// and the job table only: a poll never reaches Docker or a helper.
func (h *handler) diskActions(c echo.Context, dk *ui.NodeDisk, p service.NodeDiskPanel, d service.NodeDetail, base string, online bool) error {
	forgetRun, err := h.jobActive(c, d.ID, "disk-forget")
	if err != nil {
		return err
	}
	adoptRun, err := h.jobActive(c, d.ID, "disk-adopt")
	if err != nil {
		return err
	}
	switch p.State {
	case "forgetting":
		dk.Op = "Forgetting"
		dk.Note = "Forgetting this disk."
		if !forgetRun && online {
			dk.Note = "Nothing starts on this disk until it is forgotten."
			dk.Resume = &ui.DiskResume{Action: base + "/disk/forget", Vals: ui.AdoptVals(p.ID, "", p.ID), Text: "Forgetting did not finish."}
		}
	case "adopting":
		dk.Op = "Adopting"
		dk.Note = "Adopting this disk."
		if !adoptRun && online {
			dk.Note = "Nothing starts on this disk until the adoption is done."
			dk.Resume = &ui.DiskResume{Action: base + "/disk/adopt", Vals: ui.AdoptVals(p.ID, p.Path, ""), Text: "Adopting did not finish."}
		}
	case "missing", "adding":
		addRun, err := h.jobActive(c, d.ID, kindsAdd...)
		if err != nil {
			return err
		}
		switch {
		case forgetRun:
			dk.Note = "Forgetting this disk."
		case p.State == "adding" && addRun, !online:
			if dk.Whole && addRun {
				dk.Note = "Formatting " + dk.Device + "."
				if p.Formatted {
					dk.Note = "Mounting " + dk.Device + "."
				}
			}
		default:
			dk.Note = "Disk gone for good? Forget it."
			if dk.Whole && p.State == "adding" && p.Formatted {
				dk.Note = "Try again mounts it. It is not formatted a second time. Disk gone for good? Forget it."
			}
			f, err := h.forgetDialog(c, p.ID, d.Name)
			if errors.Is(err, errs.ErrNotFound) { // the forget finished since the panel was read
				dk.Note = ""
				break
			}
			if err != nil {
				return err
			}
			f.Action = base + "/disk/forget"
			dk.Forget = &f
		}
	case "ready":
		if !online {
			break
		}
		switch {
		case p.MoveLive:
			dk.MoveAll = &ui.DiskMoveAll{Disabled: true, Note: "A move is running."}
		case p.InDocker > 0:
			dk.MoveAll = &ui.DiskMoveAll{Plan: base + "/disk/move-all/plan", Note: inDockerNote(p.InDocker)}
		}
		var err error
		if dk.MoveJobs, err = h.moveJobs(c, d.ID); err != nil {
			return err
		}
	}
	return nil
}

func inDockerNote(n int) string {
	if n == 1 {
		return "1 volume is still in Docker storage."
	}
	return fmt.Sprintf("%d volumes are still in Docker storage.", n)
}

// forgetDialog is the Forget dialog's data for a disk: the volumes the forget
// restores, and what it would refuse with now. node is the typed word.
func (h *handler) forgetDialog(c echo.Context, diskID, node string) (ui.DiskForget, error) {
	fp, err := h.orch.PlanDiskForget(c.Request().Context(), diskID)
	if err != nil {
		return ui.DiskForget{}, err
	}
	f := ui.DiskForget{Node: fp.Node, Disk: diskID, Gone: fp.Gone, Refusal: fp.Refusal, Word: cmp.Or(node, diskID)}
	if fp.Gone {
		f.Word = diskID
	}
	f.Vals = ui.AdoptVals(diskID, "", f.Word)
	for _, v := range fp.Volumes {
		r := ui.ForgetVolume{Name: v.Slug, Where: v.Where, Note: "starts empty", Empty: true}
		if v.HasBackup {
			r.Note, r.Empty = "backup of "+v.Backup.Format("Jan 2"), false
		}
		f.Volumes = append(f.Volumes, r)
	}
	return f, nil
}

// moveJobs are the volume-to-disk jobs of the node: the live ones, and those
// that ended in the last ten minutes, newest first.
func (h *handler) moveJobs(c echo.Context, nodeID string) ([]ui.MoveJobRow, error) {
	ctx := c.Request().Context()
	js, err := h.orch.Jobs(ctx, 100)
	if err != nil {
		return nil, err
	}
	var out []ui.MoveJobRow
	for _, j := range js {
		var p struct {
			NodeID  string `json:"node_id"`
			Volumes []struct {
				VolumeID string `json:"volume_id"`
			} `json:"volumes"`
		}
		if j.Kind != "volume-to-disk" || json.Unmarshal([]byte(j.Payload), &p) != nil || p.NodeID != nodeID {
			continue
		}
		if j.FinishedAt != nil && time.Since(*j.FinishedAt) > 10*time.Minute {
			continue
		}
		var names []string
		for _, v := range p.Volumes {
			if x, err := h.orch.Volume(ctx, v.VolumeID); err == nil {
				names = append(names, x.Slug)
			}
		}
		state := j.State
		if state == "waiting" {
			state = "queued"
		}
		jv := render.JobViewNote(ctx, h.orch, "", j)
		out = append(out, ui.MoveJobRow{
			Names: strings.Join(names, ", "), State: state, Msg: cmp.Or(jv.Error, jv.Note), Failed: j.State == "failed",
			Href: jv.Href,
		})
		if len(out) == 8 {
			break
		}
	}
	return out, nil
}

// xfsDisk fills what only a disk made from a whole device has: the device, a
// device that changed, and new space to grow into. Rows and the job table
// only: a poll never reaches Docker or a helper.
func (h *handler) xfsDisk(c echo.Context, dk *ui.NodeDisk, p service.NodeDiskPanel, d service.NodeDetail, base string, online bool) error {
	xfsView(dk, p, base, online)
	if dk.Grow == "" {
		return nil
	}
	var err error
	dk.Growing, err = h.jobActive(c, d.ID, "disk-grow")
	return err
}

// xfsView is the xfs part of the section from the panel's rows alone.
func xfsView(dk *ui.NodeDisk, p service.NodeDiskPanel, base string, online bool) {
	dk.Whole, dk.Formatted = true, p.Formatted
	dk.Device = devLabel(p.Device)
	now, was := p.DeviceBytes, p.WasBytes
	if now == 0 {
		now = was
	}
	if now > 0 {
		dk.DeviceSize = diskSize(int64(now))
	}
	if p.State != "ready" {
		return
	}
	switch p.Damaged {
	case "smaller":
		dk.DeviceWas = diskSize(int64(was))
		dk.Damaged = dk.Device + " is now " + dk.DeviceSize + ", was " + dk.DeviceWas + ". Data on it may be damaged. Make the device " + dk.DeviceWas + " again, then check the disk."
	case "other_fs":
		dk.Damaged = "Another filesystem is on " + dk.Device + " now. The disk's data may be gone."
	}
	if p.Damaged == "" && p.NewSpace > 0 && online {
		dk.NewSpace = diskSize(int64(p.NewSpace))
		dk.Grow = base + "/disk/grow"
	}
}

// devLabel is a device as the card names it: sdb for /dev/sdb, an id as it is.
func devLabel(id string) string {
	if strings.HasPrefix(id, "/dev/") {
		return strings.TrimPrefix(id, "/dev/")
	}
	return id
}
