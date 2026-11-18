package env

import (
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/volume"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The volume's size cap in the drawer. The Settings tab edits it in three
// steps that all answer the drawer: the pencil (GET ?cap=edit), a change in
// the field (POST cap/stage: nothing is saved, the footer says what changes)
// and Apply (POST cap). A cap is MB in the service; the field is whole MB or
// GB.

// capReq is the cap card's state as an action leaves it for volume().
type capReq struct {
	mode, size, unit, err string
	mb                    *int64
}

const capKey = "volcap"

// capText is a cap as people read it: 20 GB, 1500 MB, 2 TB.
func capText(mb *int64) string {
	switch {
	case mb == nil:
		return "No cap"
	case *mb%(1<<20) == 0:
		return strconv.FormatInt(*mb>>20, 10) + " TB"
	case *mb%1024 == 0:
		return strconv.FormatInt(*mb>>10, 10) + " GB"
	}
	return strconv.FormatInt(*mb, 10) + " MB"
}

// capField is the field a cap fills: the unit kept from the stored value.
func capField(mb *int64) (size, unit string) {
	switch {
	case mb == nil:
		return "", "GB"
	case *mb%1024 == 0:
		return strconv.FormatInt(*mb>>10, 10), "GB"
	}
	return strconv.FormatInt(*mb, 10), "MB"
}

// parseCap reads the field. Empty is no cap; anything else is whole MB or GB
// and goes through the service's own refusal, so the panel never words a
// limit the service does not.
func parseCap(size, unit string) (*int64, error) {
	size = strings.TrimSpace(size)
	if size == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		n = 0
	}
	if unit == "GB" {
		if n > 1<<40 {
			n = 1 << 62 // far over the limit, and no overflow
		} else {
			n *= 1024
		}
	}
	return &n, service.CheckVolumeCap(&n)
}

func sameCap(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// capNote is why a cap does nothing, "" = it is a limit.
func capNote(enforced bool, st *volume.Storage) string {
	switch {
	case enforced || st == nil:
		return ""
	case st.Disk:
		return "Not enforced on a folder disk."
	}
	return "Not enforced in Docker storage."
}

// capFact is the Overview's cap fact.
func (h *handler) capFact(c echo.Context, vol service.Volume, st *volume.Storage, enforced bool) *volume.CapFact {
	f := &volume.CapFact{Set: vol.MaxSizeMB != nil, Value: capText(vol.MaxSizeMB), Note: capNote(enforced, st)}
	if st != nil && st.SizeBytes >= 0 {
		f.Used, f.Tone = render.Size(st.SizeBytes), "accent"
		if vol.MaxSizeMB != nil {
			capB := *vol.MaxSizeMB << 20
			f.Pct = int(min(100, st.SizeBytes*100/capB))
			switch {
			case st.SizeBytes >= capB:
				f.Tone = "danger"
			case f.Pct >= 90:
				f.Tone = "warn"
			}
		}
	}
	if can(c, scope(c), "tile.write") {
		f.Edit, f.Node = render.EnvURL(c)+"/-/volumes/"+vol.ID+"?tab=settings", vol.ID
	}
	return f
}

// capCard is the Settings tab's cap card; req is what an action left.
func (h *handler) capCard(c echo.Context, vol service.Volume, st *volume.Storage, enforced bool) *volume.CapCard {
	req, _ := c.Get(capKey).(capReq)
	base := render.EnvURL(c) + "/-/volumes/" + vol.ID
	k := &volume.CapCard{Mode: "read", Value: capText(vol.MaxSizeMB), Can: can(c, scope(c), "tile.write"), Base: base, Note: capNote(enforced, st)}
	if !k.Can {
		return k
	}
	if req.mode == "" && c.QueryParam("cap") == "edit" {
		req = capReq{mode: "edit", size: c.QueryParam("size"), unit: c.QueryParam("unit")}
		if _, set := c.QueryParams()["size"]; !set {
			req.size, req.unit = capField(vol.MaxSizeMB)
		}
	}
	switch req.mode {
	case "edit":
		k.Mode, k.Size, k.Unit, k.Err = "edit", req.size, req.unit, req.err
	case "pending":
		k.Mode, k.Value, k.Was = "pending", capText(req.mb), capText(vol.MaxSizeMB)
		k.Size, k.Unit, k.Err = req.size, req.unit, req.err
		if req.mb != nil {
			k.MB = strconv.FormatInt(*req.mb, 10)
		}
		switch {
		case enforced:
			k.Impact = "Applies at once. No restart."
		case st != nil && st.Disk:
			k.Impact = "Saved, not enforced on a folder disk."
		default:
			k.Impact = "Saved, not enforced in Docker storage."
		}
	}
	return k
}

// POST …/-/volumes/:volume/cap/stage: the field changed. A refusal keeps the
// field open; a new cap shows its footer; the saved cap shows the card as it
// was. Nothing is saved.
func (h *handler) CapStage(c echo.Context) error {
	vol, err := h.orch.Volume(c.Request().Context(), c.Param("volume"))
	if err != nil {
		return h.volume(c, "settings", "", err)
	}
	size, unit := strings.TrimSpace(c.FormValue("size")), c.FormValue("unit")
	mb, err := parseCap(size, unit)
	switch {
	case err != nil:
		msg, _ := render.Refused(err)
		c.Set(capKey, capReq{mode: "edit", size: size, unit: unit, err: msg})
	case !sameCap(mb, vol.MaxSizeMB):
		size, unit = capField(mb)
		c.Set(capKey, capReq{mode: "pending", size: size, unit: unit, mb: mb})
	}
	return h.volume(c, "settings", "", nil)
}

// POST …/-/volumes/:volume/cap: Apply. A refusal (a cap under what the volume
// holds) leaves the staged cap with the reason in its footer; nothing changed.
func (h *handler) SetCap(c echo.Context) error {
	var mb *int64
	if raw := strings.TrimSpace(c.FormValue("max_size_mb")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			n = 0 // the service words it
		}
		mb = &n
	}
	err := h.orch.SetVolumeCap(c.Request().Context(), c.Param("volume"), mb)
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return h.volume(c, "settings", "", err)
		}
		size, unit := capField(mb)
		c.Set(capKey, capReq{mode: "pending", size: size, unit: unit, mb: mb, err: msg})
		return h.volume(c, "settings", "", nil)
	}
	note := "Size cap set."
	if mb == nil {
		note = "Size cap removed."
	}
	return h.volume(c, "settings", note, nil)
}
