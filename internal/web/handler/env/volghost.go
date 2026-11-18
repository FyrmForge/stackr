package env

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/ui/drawer/volume"
)

// The two Discard verbs of a ghost volume. The banner and its typed confirm
// are in the drawer (vol_ghost.templ); these are the presses.

// POST …/-/volumes/:volume/discard-copy: remove the plain copy, keep the folder.
func (h *handler) DiscardCopy(c echo.Context) error {
	err := h.orch.DiscardPlainCopy(c.Request().Context(), middleware.Principal(c), c.Param("volume"), c.FormValue("confirm"))
	return h.volume(c, "overview", "the Docker storage copy is gone", err)
}

// POST …/-/volumes/:volume/discard-object: remove an object bound elsewhere.
func (h *handler) DiscardObject(c echo.Context) error {
	err := h.orch.DiscardBoundObject(c.Request().Context(), middleware.Principal(c), c.Param("volume"), c.FormValue("confirm"))
	return h.volume(c, "overview", "the wrong link is gone", err)
}

// ghost is the banner of the volume's drawer: two copies, or an object bound
// to the wrong folder. nil when the last gate decision found none. Memory
// only. The button is the tile writer's; the paths are the node admin's.
func (h *handler) ghost(c echo.Context, volumeID, name string, write bool) *volume.GhostView {
	st, err := h.orch.VolumeStorage(c.Request().Context(), volumeID)
	if err != nil || st.Ghost == "" {
		return nil
	}
	g := &volume.GhostView{Bound: st.Ghost == "bound"}
	if !write {
		return g
	}
	s := scope(c)
	base := "/" + s.Org.Slug + "/" + s.Stack.Slug + "/" + s.Env.Slug + "/-/volumes/" + volumeID
	action, points, should := base+"/discard-copy", "", ""
	if g.Bound {
		action = base + "/discard-object"
		if p := middleware.Principal(c); p != nil && authz.Can(p.Access, "node.manage", authz.Resource{}) == nil {
			points, should = st.Device, st.Folder
		}
	}
	g.Discard = volume.GhostDiscard(action, name, s.Stack.Name+" / "+s.Env.Name, g.Bound, points, should)
	return g
}
