package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Storage tiles (§2.7) live on the server page: create/delete shares & pools,
// declare sub-paths, probe. Attachments are edited on the consumer tile.

// probeAndRecord probes the share root through a throwaway sub-path volume
// and stores the outcome on the storage row. The transient volume is removed
// either way, real sub-path volumes are created on declaration/deploy.
func (h *handler) probeAndRecord(ctx context.Context, st *repo.Storage) {
	// On the storage's own server. Probing the manager for a share the
	// operator hung off a worker proved the manager could reach it and said
	// nothing about the machine that will mount it at deploy time.
	node, err := h.clus.NodeOfStorage(ctx, st)
	if err == nil {
		p := repo.StoragePath{ID: uuid.New().String(), StorageID: st.ID, Subpath: ""}
		err = storagetiles.Probe(ctx, h.clus, node, st, &p)
		_ = h.clus.RemoveVolume(ctx, node, repo.StorageVolume(p.ID))
	}
	if err != nil {
		st.Status, st.StatusMsg = "error", err.Error()
	} else {
		st.Status, st.StatusMsg = "ok", ""
	}
	if err := h.store.UpdateStorage(ctx, st); err != nil {
		slog.Error("storage probe result not saved", "storage", st.ID, "status", st.Status, "error", err)
	}
}

// POST /servers/:id/storage
func (h *handler) CreateStorage(c echo.Context) error {
	// ServerID is this page's node. The API hard-coded "local", which is why
	// every pool created from the CLI probed and mounted on the manager.
	st, err := h.storage.Create(c.Request().Context(), service.StorageSpec{
		Name:     c.FormValue("name"),
		Backend:  c.FormValue("backend"),
		Address:  c.FormValue("address"),
		Export:   c.FormValue("export"),
		ServerID: c.Param("id"),
		Username: c.FormValue("username"),
		Password: c.FormValue("password"),
		Opts:     c.FormValue("opts"),
	})
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if st.Status == "ok" {
		middleware.SetFlash(c, "Storage "+st.Name+" created and probed OK. Declare sub-paths to make it attachable.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Storage "+st.Name+" created but the probe failed: "+st.StatusMsg, middleware.FlashError)
	}
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/storage/delete
func (h *handler) DeleteStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := h.storage.Get(ctx, c.FormValue("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	// Through the service, which has the org branch this handler never had:
	// an org share is referenced as ${{ org.storage.NAME }}, which the
	// slug/path scan here could not see, and its volumes live on every node
	// that mounted it rather than on one.
	if err := h.storage.Delete(ctx, st); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Storage "+st.Name+" removed. Data on the share/pool itself is untouched.", middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/storage/probe
func (h *handler) ProbeStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := h.storage.Get(ctx, c.FormValue("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	h.probeAndRecord(ctx, st)
	if st.Status == "ok" {
		middleware.SetFlash(c, "Probe OK.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Probe failed: "+st.StatusMsg, middleware.FlashError)
	}
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/storage/paths
func (h *handler) CreateStoragePath(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := h.storage.Get(ctx, c.FormValue("storage_id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	// The service slugifies and then checks for emptiness. This handler did
	// it the other way round, so a name of "..." became a sub-path called "".
	p, err := h.storage.DeclarePath(ctx, st, c.FormValue("name"), c.FormValue("subpath"), c.FormValue("forced_ro") != "")
	switch {
	case p == nil:
		return stackrmw.HTTP(err)
	case err != nil:
		// Declared, but not mountable: the row is real and the message says why.
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
	default:
		middleware.SetFlash(c, "Sub-path "+p.Name+" ready. Attach it as "+st.Slug+"/"+p.Name+":/mount from a tile's settings.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/storage/paths/delete
func (h *handler) DeleteStoragePath(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.store.GetStoragePath(ctx, c.FormValue("id"))
	if err != nil || p == nil {
		return echo.NewHTTPError(http.StatusNotFound, "sub-path not found")
	}
	st, _ := h.storage.Get(ctx, p.StorageID)
	if st != nil {
		if consumers, _ := h.storage.Consumers(ctx, st, p.Name); len(consumers) > 0 {
			return echo.NewHTTPError(http.StatusConflict, "still attached to "+strings.Join(consumers, ", ")+"; detach first")
		}
	}
	if node, err := h.clus.NodeOfStorage(ctx, st); err == nil {
		_ = h.clus.RemoveVolume(ctx, node, repo.StorageVolume(p.ID))
	}
	if err := h.store.DeleteStoragePath(ctx, p.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Sub-path removed. Data on the share/pool is untouched.", middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}
