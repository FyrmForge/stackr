package admin

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
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
		Dialog: ui.DiskDialog{Probe: base + "/disk/probe", State: base + "/disk/state", Add: base + "/disk/add"},
	}
	online := d.Status == "online"
	if !p.Has {
		dk.Help = "Keep volumes on their own disk, apart from the system disk."
		switch {
		case p.Refusal != "":
			dk.Help = p.Refusal
		case !online:
			dk.Why = "The node is offline."
		case !d.Controller && (live.Facts == nil || !live.Facts.DisksRead):
			dk.Help = "Update this node to add a disk."
		default:
			dk.Add = base + "/disk/add"
		}
		return dk, nil
	}
	if p.State == "ready" {
		dk.Help = "New volumes on this node are stored here."
		if p.Total > 0 {
			dk.Used = diskSize(int64(p.Total-p.Free)) + " of " + diskSize(int64(p.Total))
			dk.Pct = strconv.Itoa(int((p.Total - p.Free) * 100 / p.Total))
			dk.Low = p.Low
		}
	}
	if !p.MissingSince.IsZero() {
		dk.Since = ago(p.MissingSince)
	}
	if p.State != "ready" && online {
		dk.Check = base + "/disk/check"
		if dk.Checking, err = h.jobActive(c, d.ID, "disk-check"); err != nil {
			return dk, err
		}
	}
	if p.State == "adding" && online {
		adding, err := h.jobActive(c, d.ID, "disk-add")
		if err != nil {
			return dk, err
		}
		if dk.AddFailed = !adding; dk.AddFailed {
			dk.Add = base + "/disk/add"
			dk.Dialog.Path = p.Path
		}
	}
	for i, r := range p.Volumes {
		row := ui.DiskVolRow{Name: r.Slug, Where: r.Where, Size: "unknown"}
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

// jobActive is whether a job of kind for the node is queued or running.
func (h *handler) jobActive(c echo.Context, nodeID, kind string) (bool, error) {
	js, err := h.orch.Jobs(c.Request().Context(), 0, "queued", "waiting", "running")
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(js, func(j service.Job) bool {
		var p struct {
			NodeID string `json:"node_id"`
		}
		return j.Kind == kind && json.Unmarshal([]byte(j.Payload), &p) == nil && p.NodeID == nodeID
	}), nil
}

// diskSize is a disk size as the section says it: 86 MB, 4.2 GB, 278 GB, 1.8 TB.
func diskSize(b int64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(b)/(1<<40))
	case b >= 10<<30:
		return fmt.Sprintf("%.0f GB", float64(b)/(1<<30))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%d KB", b>>10)
	}
	return fmt.Sprintf("%d B", b)
}

// diskDialog is the dialog's fields as posted.
func diskDialog(c echo.Context) ui.DiskDialog {
	base := nodesBase + "/" + c.Param("node")
	return ui.DiskDialog{
		Probe: base + "/disk/probe", State: base + "/disk/state", Add: base + "/disk/add",
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
		if strings.Contains(msg, "no such file") {
			return "No such folder."
		}
		return "Not a mount point. Mount the disk there first."
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
	switch {
	case d.Path == "":
		d.Msg = "Give a full path to a mounted folder, like /mnt/data."
	case !strings.HasPrefix(d.Path, "/"):
		d.Msg = "Start the path with /."
	default:
		p, err := h.orch.ProbeNodeFolder(ctx, middleware.Principal(c), c.Param("node"), d.Path)
		var down *service.NodeDownError
		onField, top, refused := render.OnField(err, "path")
		switch {
		case err != nil && (errors.As(err, &down) || !refused):
			name := c.Param("node")
			if n, nerr := h.orch.NodeDetail(ctx, name); nerr == nil {
				name = n.Name
			}
			d.Msg = name + " did not answer. Try again."
		case err != nil:
			d.Msg = cmp.Or(onField, top)
		case p.Code != "":
			d.Msg = probeText(p.Code, p.Message)
		default:
			d.Msg, d.Tone, d.Invalid = "Mounted, empty, "+diskSize(int64(p.Free))+" free.", "ok", false
			if p.Network {
				d.Msg += " Network disk (" + p.FSType + ")."
			}
			d.Checked, d.Network, d.NetOK = d.Path, p.Network, false
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
