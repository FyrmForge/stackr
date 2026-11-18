package admin

import (
	"cmp"
	"net/http"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Adopt disk in the node drawer. The dialog is an answer into #adopt-dlg: the
// plan POST (explicit, it asks the node about the folder) draws step one or
// the plan, the discard POST removes a copy in the way and draws the plan
// again, the adopt POST queues the job and answers the drawer.

// adoptDialog is the dialog for the node and path: step one when path is
// empty or the folder cannot be adopted, the plan when it can. msg is a
// refusal that came with the press.
func (h *handler) adoptDialog(c echo.Context, path, msg string) (ui.AdoptDialog, error) {
	ctx := c.Request().Context()
	base := nodesBase + "/" + c.Param("node")
	a := ui.AdoptDialog{PlanURL: base + "/disk/adopt/plan", Adopt: base + "/disk/adopt", Path: path, Msg: msg}
	p, err := h.orch.PlanDiskAdopt(ctx, c.Param("node"), path)
	if err != nil {
		m, ok := render.Refused(err)
		if !ok {
			return a, err
		}
		a.Msg = m
	}
	for _, k := range p.Candidates {
		f, err := h.forgetDialog(c, k.DiskID, "")
		if err != nil {
			return a, err
		}
		f.Action = base + "/disk/forget"
		a.Cands = append(a.Cands, ui.AdoptCand{Disk: k.DiskID, Was: k.Node, Forget: f})
	}
	if err != nil {
		return a, nil
	}
	if p.DiskID == "" || (p.Refusal != "" && !p.CopiesOnly) {
		if p.Refusal != "" {
			a.Msg = cmp.Or(msg, adoptRefusalText(p))
		}
		return a, nil
	}
	a.Msg = ""
	a.View = adoptPlanView(base, path, p, msg)
	return a, nil
}

func adoptPlanView(base, path string, p service.DiskAdoptPlan, msg string) *ui.AdoptPlanView {
	v := &ui.AdoptPlanView{Sub: path, Node: p.Node, Msg: msg, Vals: ui.AdoptVals(p.DiskID, path, p.Node), Unclaimed: p.Unclaimed}
	if p.OldNode != "" {
		v.Sub += ", from " + p.OldNode + " (gone)"
	}
	where := map[string]string{}
	for _, e := range p.Envs {
		tiles := "1 tile"
		if e.Tiles != 1 {
			tiles = strconv.Itoa(e.Tiles) + " tiles"
		}
		v.Envs = append(v.Envs, ui.DlgPair{Name: e.Label, Where: tiles, Note: "moves to " + p.Node})
	}
	for _, x := range p.OnDisk {
		where[x.ID] = x.Where
		v.OnDisk = append(v.OnDisk, ui.DlgPair{Name: x.Slug, Where: x.Where, Note: "stays on the disk"})
	}
	for _, x := range p.Elsewhere {
		where[x.ID] = x.Where
		r := ui.DlgPair{Name: x.Slug, Where: x.Where, Note: "starts empty", Warn: true}
		if x.HasBackup {
			r.Note, r.Warn = "restored from backup of "+x.Backup.Format("Jan 2"), false
		}
		v.Elsewhere = append(v.Elsewhere, r)
	}
	for _, k := range p.Copies {
		row := ui.AdoptCopyRow{Name: k.Slug, Where: where[k.VolumeID], Note: "copy in Docker storage"}
		body := "Deletes the copy in Docker storage. The copy on the node disk stays."
		opener, title, button := "Discard", "Discard Docker storage copy", "Discard copy"
		if k.Bound {
			row.Note = "bound to another folder"
			body = "Removes the link only. Both folders keep their data."
			opener, title, button = "Remove link", "Remove the wrong link", "Remove link"
		}
		row.Discard = comp.DiskConfirmView{
			Opener: opener, Link: true, Title: title,
			Sub:  strings.Join(nonEmpty(k.Slug, where[k.VolumeID], "on "+p.Node), ", "),
			Word: k.Slug, Button: button, Action: base + "/disk/adopt/discard-copy", Target: "#adopt-dlg",
			Vals: ui.DiscardVals(p.DiskID, path, k.VolumeID, k.Slug), Body: ui.TextBody(body),
		}
		v.Copies = append(v.Copies, row)
	}
	return v
}

func nonEmpty(s ...string) []string {
	var out []string
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// adoptRefusalText is the plan's refusal in the words of the dialog. The
// service's own sentence stays for the ones it says well.
func adoptRefusalText(p service.DiskAdoptPlan) string {
	r := p.Refusal
	switch {
	case strings.HasPrefix(r, "The disk's node, "):
		name, rest, _ := strings.Cut(strings.TrimPrefix(r, "The disk's node, "), ", ")
		if strings.HasPrefix(rest, "is still online") {
			return name + " is still online. Forget the node first."
		}
		return name + " is offline, not forgotten. Forget the node first."
	case r == "Remove the standbys first." && len(p.Hosts) > 0:
		h, node := p.Hosts[0], cmp.Or(p.OldNode, "The old node")
		if h.Primary {
			return node + " runs the primary of " + h.Env + ". Move it back first."
		}
		return node + " hosts a standby of " + h.Env + " " + h.Tile + ". Remove the standby first."
	case r == "This folder carries no disk this panel knows.":
		return "This folder is not a stackr disk."
	case p.AdoptedBy != "":
		return "This disk is being adopted by " + p.AdoptedBy + "."
	}
	return r
}

// POST …/nodes/:node/disk/adopt/plan: what the adopt would do.
func (h *handler) diskAdoptPlan(c echo.Context) error {
	a, err := h.adoptDialog(c, strings.TrimSpace(c.FormValue("path")), "")
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, ui.AdoptDialogView(a))
}

// POST …/nodes/:node/disk/adopt/discard-copy: remove a plain copy that blocks
// the adopt, then draw the plan again.
func (h *handler) diskAdoptDiscard(c echo.Context) error {
	path := strings.TrimSpace(c.FormValue("path"))
	err := h.orch.DiscardAdoptCopy(c.Request().Context(), middleware.Principal(c), c.Param("node"),
		path, c.FormValue("disk"), c.FormValue("volume"), c.FormValue("confirm"))
	msg := ""
	if err != nil {
		var ok bool
		if msg, ok = render.Refused(err); !ok {
			return middleware.HTTPError(err)
		}
	}
	a, err := h.adoptDialog(c, path, msg)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, ui.AdoptDialogView(a))
}

// POST …/nodes/:node/disk/adopt: adopt, or resume an adopting row. A refusal
// of the dialog's press lands in the dialog; the strip's Resume answers the
// drawer.
func (h *handler) diskAdopt(c echo.Context) error {
	path := strings.TrimSpace(c.FormValue("path"))
	j, err := h.orch.AdoptDisk(c.Request().Context(), middleware.Principal(c), c.Param("node"),
		c.FormValue("disk"), path, c.FormValue("confirm"))
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		if c.FormValue("confirm") == "" {
			return h.nodeAnswer(c, "overview", "", err)
		}
		a, err := h.adoptDialog(c, path, msg)
		if err != nil {
			return middleware.HTTPError(err)
		}
		c.Response().Header().Set("HX-Retarget", "#adopt-dlg")
		c.Response().Header().Set("HX-Reswap", "outerHTML")
		return respond.HTML(c, http.StatusOK, ui.AdoptDialogView(a))
	}
	c.Set("job", j)
	return h.nodeAnswer(c, "overview", "Adopting the disk.", nil)
}
