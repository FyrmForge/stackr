package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
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

// attachedConsumers lists tiles whose storage lines reference this storage
// (optionally one sub-path). scans every tile, tens of rows.
func (h *handler) attachedConsumers(ctx context.Context, slug, pathName string) []string {
	tiles, err := h.store.ListTiles(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for i := range tiles {
		for _, l := range strings.Split(tiles[i].Storage, "\n") {
			s, p, _, _, err := storagetiles.ParseAttachment(strings.TrimSpace(l))
			if err != nil || s != slug {
				continue
			}
			if pathName == "" || p == pathName {
				out = append(out, tiles[i].Name)
			}
		}
	}
	return out
}

// POST /servers/:id/storage
func (h *handler) CreateStorage(c echo.Context) error {
	ctx := c.Request().Context()
	name := strings.TrimSpace(c.FormValue("name"))
	backend := c.FormValue("backend")
	if name == "" || !storagetiles.ValidBackend(backend) {
		return echo.NewHTTPError(http.StatusBadRequest, "name and a backend (nfs, smb or local) required")
	}
	slug := repo.Slugify(name)
	if existing, _ := h.store.GetStorageBySlug(ctx, slug); existing != nil {
		return echo.NewHTTPError(http.StatusConflict, "storage "+slug+" already exists")
	}
	st := &repo.Storage{
		ID: uuid.New().String(), ServerID: c.Param("id"), Name: name, Slug: slug,
		Backend: backend, Address: strings.TrimSpace(c.FormValue("address")),
		Export: strings.TrimSpace(c.FormValue("export")), Username: c.FormValue("username"),
		Password: c.FormValue("password"), Opts: strings.TrimSpace(c.FormValue("opts")),
		Status: "unknown", CreatedAt: time.Now().UTC(),
	}
	if st.Backend == "local" && !strings.HasPrefix(st.Export, "/") {
		return echo.NewHTTPError(http.StatusBadRequest, "a local pool needs an absolute host path")
	}
	if st.Backend != "local" && st.Address == "" {
		return echo.NewHTTPError(http.StatusBadRequest, backend+" storage needs an address")
	}
	if err := h.store.CreateStorage(ctx, st); err != nil {
		return err
	}
	h.probeAndRecord(ctx, st)
	if st.Status == "ok" {
		middleware.SetFlash(c, "Storage "+name+" created and probed OK. Declare sub-paths to make it attachable.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Storage "+name+" created but the probe failed: "+st.StatusMsg, middleware.FlashError)
	}
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/storage/delete
func (h *handler) DeleteStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := h.store.GetStorage(ctx, c.FormValue("id"))
	if err != nil || st == nil {
		return echo.NewHTTPError(http.StatusNotFound, "storage not found")
	}
	if consumers := h.attachedConsumers(ctx, st.Slug, ""); len(consumers) > 0 {
		return echo.NewHTTPError(http.StatusConflict, "still attached to "+strings.Join(consumers, ", ")+"; detach first")
	}
	paths, _ := h.store.ListStoragePaths(ctx, st.ID)
	if node, err := h.clus.NodeOfStorage(ctx, st); err == nil {
		for i := range paths {
			_ = h.clus.RemoveVolume(ctx, node, repo.StorageVolume(paths[i].ID))
		}
	}
	if err := h.store.DeleteStorage(ctx, st.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Storage "+st.Name+" removed. Data on the share/pool itself is untouched.", middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/storage/probe
func (h *handler) ProbeStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := h.store.GetStorage(ctx, c.FormValue("id"))
	if err != nil || st == nil {
		return echo.NewHTTPError(http.StatusNotFound, "storage not found")
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
	st, err := h.store.GetStorage(ctx, c.FormValue("storage_id"))
	if err != nil || st == nil {
		return echo.NewHTTPError(http.StatusNotFound, "storage not found")
	}
	name := repo.Slugify(strings.TrimSpace(c.FormValue("name")))
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "sub-path name required")
	}
	sub := strings.Trim(strings.TrimSpace(c.FormValue("subpath")), "/")
	if strings.Contains(sub, "..") {
		return echo.NewHTTPError(http.StatusBadRequest, "sub-path must stay inside the share")
	}
	p := &repo.StoragePath{
		ID: uuid.New().String(), StorageID: st.ID, Name: name, Subpath: sub,
		ForcedRO: c.FormValue("forced_ro") != "", CreatedAt: time.Now().UTC(),
	}
	if err := h.store.CreateStoragePath(ctx, p); err != nil {
		return err
	}
	// Materialize + probe now so a typo'd sub-path fails here, not at deploy.
	node, err := h.clus.NodeOfStorage(ctx, st)
	if err != nil {
		middleware.SetFlash(c, "Sub-path declared, but "+err.Error(), middleware.FlashError)
		return respond.Redirect(c, "/servers/"+c.Param("id"))
	}
	if err := storagetiles.Probe(ctx, h.clus, node, st, p); err != nil {
		middleware.SetFlash(c, "Sub-path declared, but mounting it failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Sub-path "+name+" ready. Attach it as "+st.Slug+"/"+name+":/mount from a tile's settings.", middleware.FlashSuccess)
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
	st, _ := h.store.GetStorage(ctx, p.StorageID)
	if st != nil {
		if consumers := h.attachedConsumers(ctx, st.Slug, p.Name); len(consumers) > 0 {
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
