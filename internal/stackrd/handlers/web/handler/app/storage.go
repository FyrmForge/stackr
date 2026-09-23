package app

import (
	"context"
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Storage attachments (§2.7). The write surface is the tile's storage column,
// (same field config-as-code owns) so on a config-managed stack the file
// owns attachments and this UI is read-only.

func (h *handler) storageRows(a *repo.Tile) []storageRow {
	var rows []storageRow
	for _, l := range splitNonEmpty(a.Storage) {
		slug, name, mount, ro, err := storagetiles.ParseAttachment(l)
		if err != nil {
			continue
		}
		rows = append(rows, storageRow{Line: l, Source: slug + "/" + name, Mount: mount, RO: ro})
	}
	return rows
}

func (h *handler) storageOptions(ctx context.Context, a *repo.Tile) []storageOption {
	storages, err := h.storage.ListAll(ctx)
	if err != nil {
		return nil
	}
	attached := map[string]bool{}
	for _, r := range h.storageRows(a) {
		attached[r.Source] = true
	}
	var out []storageOption
	for i := range storages {
		st := &storages[i]
		paths, _ := h.storage.Paths(ctx, st.ID)
		for j := range paths {
			val := st.Slug + "/" + paths[j].Name
			if attached[val] {
				continue
			}
			o := storageOption{Value: val, Label: val + " (" + st.Backend + ")"}
			if err := storagetiles.ValidateAttach(st, a); err != nil {
				o.Blocked = "databases need local-backed storage"
			}
			out = append(out, o)
		}
	}
	return out
}

// GET /apps/:id/storage
func (h *handler) StorageFrag(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	return respond.HTML(c, http.StatusOK, storageFrag(c, a, h.storageRows(a), h.storageOptions(ctx, a)))
}

// POST /apps/:id/storage, attach one declared sub-path.
func (h *handler) AttachStorage(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	// storage: is config-modeled, a config-managed stack's file owns it.
	if err := stackrmw.RequireUnmanaged(c, h.store, a.StackID); err != nil {
		return err
	}
	source := c.FormValue("source")
	mount := strings.TrimSpace(c.FormValue("mount"))
	line := source + ":" + mount
	if c.FormValue("ro") != "" {
		line += ":ro"
	}
	for _, existing := range splitNonEmpty(a.Storage) {
		if existing == line {
			return echo.NewHTTPError(http.StatusConflict, "already attached")
		}
	}
	a.Storage = strings.TrimSpace(a.Storage + "\n" + line)
	// Through the tile service: the grammar, the "does that share exist"
	// lookup, the local-backed rule for a managed instance and the kind rule
	// are all in its validator, and it decides the redeploy. This handler had
	// its own copy of the first three and no kind rule at all.
	if _, err := h.tiles.Update(ctx, a, nil, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return respond.HTML(c, http.StatusOK, storageFrag(c, a, h.storageRows(a), h.storageOptions(ctx, a)))
}

// POST /apps/:id/storage/detach
func (h *handler) DetachStorage(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := stackrmw.RequireUnmanaged(c, h.store, a.StackID); err != nil {
		return err
	}
	target := c.FormValue("line")
	var kept []string
	for _, l := range splitNonEmpty(a.Storage) {
		if l != target {
			kept = append(kept, l)
		}
	}
	a.Storage = strings.Join(kept, "\n")
	if _, err := h.tiles.Update(ctx, a, nil, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return respond.HTML(c, http.StatusOK, storageFrag(c, a, h.storageRows(a), h.storageOptions(ctx, a)))
}
