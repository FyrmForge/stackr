package admin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Add disk, whole disk: the choice, the picker, Continue and the format. The
// helper runs in diskChoose with how=disk (the choice and Refresh) and in the
// job a format queues: never on a GET, a poll, a pick or Continue. The disk
// goes back whole as the list gave it, so a disk that changed is caught.

// diskWhy is the picker's reason a disk is greyed, as the panel words it.
func diskWhy(blocked string) string {
	switch blocked {
	case "partitions":
		return "it has partitions. Remove them on the node first"
	case "holders":
		return "it is in use by another device (dm, bcache)"
	}
	return service.DeviceBlockedWhy(blocked)
}

// deviceName is a disk as the dialog names it: sdb for /dev/sdb.
func deviceName(d service.DiskDevice) string { return cmp.Or(path.Base(d.Path), d.ID) }

// deviceChip says what a disk holds: what Format would erase.
func deviceChip(d service.DiskDevice) (label, tone string) {
	switch d.Signature {
	case "":
		return "No filesystem", ""
	case "stackr":
		return "Earlier stackr disk", "warn"
	case "table":
		return "Partition table (" + d.PTType + ")", "warn"
	}
	return cmp.Or(d.FSType, "A") + " filesystem", "warn"
}

// pickerOf is the list the picker shows, usable disks first. picked is the id
// of the disk the person had picked before a refresh.
func pickerOf(devs []service.DiskDevice, picked string) *ui.DiskPicker {
	p := &ui.DiskPicker{}
	for _, d := range devs {
		v, _ := json.Marshal(d) // a plain struct always marshals
		r := ui.DiskDevice{Name: deviceName(d), Model: d.Model, Size: diskSize(d.Bytes), Ident: cmp.Or(d.Serial, d.ID), Value: string(v)}
		if d.Blocked != "" {
			r.Why = diskWhy(d.Blocked)
		} else {
			r.Chip, r.Tone = deviceChip(d)
			r.Picked = picked != "" && d.ID == picked
		}
		p.Devices = append(p.Devices, r)
	}
	slices.SortStableFunc(p.Devices, func(a, b ui.DiskDevice) int { return cmp.Compare(boolInt(a.Why != ""), boolInt(b.Why != "")) })
	return p
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// postedDevice is the disk as the dialog posted it back.
func postedDevice(c echo.Context) (service.DiskDevice, error) {
	var d service.DiskDevice
	if err := json.Unmarshal([]byte(c.FormValue("device")), &d); err != nil || d.ID == "" {
		return d, echo.NewHTTPError(http.StatusBadRequest, "Pick a disk.")
	}
	return d, nil
}

// POST …/nodes/:node/disk/choose: the Add disk dialog's choice, or Refresh.
// how=folder draws the folder field (no helper); how=disk reads the node's
// disks. open=1 answers the whole dialog, open (Try again).
func (h *handler) diskChoose(c echo.Context) error {
	ctx := c.Request().Context()
	d := diskDialog(c)
	d.Open = c.FormValue("open") == "1"
	if n, err := h.orch.NodeDetail(ctx, c.Param("node")); err == nil {
		d.Node = n.Name
	}
	if c.FormValue("how") != "disk" {
		return respond.HTML(c, http.StatusOK, ui.DiskChoose(d))
	}
	d.How = "disk"
	var picked service.DiskDevice
	_ = json.Unmarshal([]byte(c.FormValue("device")), &picked)
	devs, err := h.orch.NodeDevices(ctx, middleware.Principal(c), c.Param("node"))
	var down *service.NodeDownError
	var he *hostdisk.Error
	switch {
	case err == nil:
		d.Picker = pickerOf(devs, picked.ID)
	case errors.As(err, &down) || service.DockerUnreachable(err) || errors.Is(err, context.DeadlineExceeded):
		d.Picker = &ui.DiskPicker{Msg: cmp.Or(d.Node, c.Param("node")) + " did not answer. Try again."}
	case errors.As(err, &he):
		d.Picker = &ui.DiskPicker{Msg: he.Message}
	default:
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		d.Picker = &ui.DiskPicker{Msg: msg}
	}
	return respond.HTML(c, http.StatusOK, ui.DiskChoose(d))
}

// POST …/nodes/:node/disk/pick: a disk was picked. No helper: only Continue
// moves, and only for a disk that can be used.
func (h *handler) diskPick(c echo.Context) error {
	d := diskDialog(c)
	d.How = "disk"
	if dev, err := postedDevice(c); err == nil && dev.Blocked == "" {
		d.Picker = &ui.DiskPicker{Devices: []ui.DiskDevice{{Picked: true}}}
	}
	return respond.HTML(c, http.StatusOK, ui.DiskPicked(d))
}

// formatOf is the Format dialog of a picked disk.
func (h *handler) formatOf(c echo.Context, dev service.DiskDevice, value string) ui.DiskFormat {
	f := ui.DiskFormat{
		Add: nodesBase + "/" + c.Param("node") + "/disk/format", Node: c.Param("node"),
		Name: deviceName(dev), Model: dev.Model, Size: diskSize(dev.Bytes), Serial: dev.Serial, ID: dev.ID,
		Anyway: dev.Signature != "", Value: value,
	}
	if n, err := h.orch.NodeDetail(c.Request().Context(), c.Param("node")); err == nil {
		f.Node = n.Name
	}
	switch dev.Signature {
	case "stackr":
		f.Holds, f.Hint = "An earlier stackr disk. Its volumes are erased.", "To keep them, adopt the disk instead."
	case "table":
		f.Holds, f.Table = "an empty partition table ("+dev.PTType+")", true
	case "filesystem":
		f.Holds = cmp.Or(dev.FSType, "a") + " filesystem"
		if dev.Label != "" {
			f.Holds += ", label " + dev.Label
		}
	}
	return f
}

// POST …/nodes/:node/disk/format/plan: Continue. Draws the Format dialog of
// the picked disk; a disk that cannot be formatted only says why.
func (h *handler) diskFormatPlan(c echo.Context) error {
	dev, err := postedDevice(c)
	if err != nil {
		return err
	}
	f := h.formatOf(c, dev, c.FormValue("device"))
	if dev.Blocked != "" {
		f.Anyway, f.Msg = false, "That disk cannot be used: "+diskWhy(dev.Blocked)+"."
	}
	return respond.HTML(c, http.StatusOK, ui.DiskFormatDialog(f))
}

// POST …/nodes/:node/disk/format: Format disk or Format anyway. The typed word
// goes to the service, which refuses without it; force is only what the
// Format anyway dialog posts. A refusal draws the dialog again with the
// reason; a queued add answers the drawer.
func (h *handler) diskFormat(c echo.Context) error {
	dev, err := postedDevice(c)
	if err != nil {
		return err
	}
	j, err := h.orch.AddNodeDevice(c.Request().Context(), middleware.Principal(c), c.Param("node"),
		dev, strings.TrimSpace(c.FormValue("typed")), c.FormValue("force") == "1")
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		f := h.formatOf(c, dev, c.FormValue("device"))
		f.Msg = msg
		c.Response().Header().Set("HX-Retarget", "#disk-fmt-dlg")
		c.Response().Header().Set("HX-Reswap", "innerHTML")
		return respond.HTML(c, http.StatusOK, ui.DiskFormatDialog(f))
	}
	c.Set("job", j)
	return h.nodeAnswer(c, "overview", "Formatting "+deviceName(dev)+".", nil)
}
