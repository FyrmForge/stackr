// Package backups is the per-tile Backups tab: the schedules on one tile,
// their run history, and the buttons that run or restore them.
//
// It is one fragment rather than two tab bodies because both tile kinds that
// can be backed up (database tiles and volume tiles) render through
// different panel packages (db and app). The tab in each is a div that fetches
// this.
package backups

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store repo.Store
	svc   *backup.Service
}

// NewHandler creates the backups-tab handler.
func NewHandler(store repo.Store, svc *backup.Service) *handler {
	return &handler{store: store, svc: svc}
}

// view is everything the fragment renders.
type view struct {
	Tile    *repo.Tile
	Kind    string // dump | volume, what this tile can produce
	Configs []configView
	Dests   []repo.BackupDestination
	VolErr  string // why this tile has no volume to back up, if it hasn't
	Slices  int    // logical dbs cut from this instance, none of which a dump covers
}

type configView struct {
	repo.Backup
	DestName string
	Runs     []repo.BackupRun
}

// loadTile resolves the tile in the URL and checks org membership. Every route
// in this file starts here, a backup resolved by id alone, with no walk back
// to the tile's org, is the hole the removed feature shipped with.
func (h *handler) loadTile(c echo.Context, id string) (*repo.Tile, error) {
	t, err := h.store.GetTile(c.Request().Context(), id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := stackrmw.RequireStackAccess(c, h.store, t.StackID); err != nil {
		return nil, err
	}
	return t, nil
}

// loadBackup resolves a backup by id through its tile's org.
func (h *handler) loadBackup(c echo.Context) (*repo.Backup, *repo.Tile, error) {
	b, err := h.store.GetBackup(c.Request().Context(), c.Param("id"))
	if err != nil || b == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	t, err := h.loadTile(c, b.TileID.String)
	if err != nil {
		return nil, nil, err
	}
	return b, t, nil
}

func (h *handler) load(c echo.Context, t *repo.Tile) (view, error) {
	ctx := c.Request().Context()
	v := view{Tile: t, Kind: repo.BackupVolume}
	if t.IsManaged() {
		v.Kind = repo.BackupDump
	}
	if _, err := backup.VolumeFor(t); err != nil {
		v.VolErr = err.Error()
	}
	orgID, err := backup.OrgOf(ctx, h.store, t)
	if err != nil {
		return v, err
	}
	dests, err := h.store.ListBackupDestinations(ctx)
	if err != nil {
		return v, err
	}
	for _, d := range dests {
		// An unshared server-wide destination is not offered here: it carries
		// the credentials the archive is written with, and the admin has not
		// handed it out.
		if d.VisibleTo(orgID) {
			v.Dests = append(v.Dests, d)
		}
	}
	// A dump covers the instance's own database only. Slices are separate
	// databases in the same server, and per-slice backups are phase 4 of
	// docs/features/shared-infra.md, so say so rather than let a 400-byte
	// artifact read as a backup of everything.
	if ps, perr := h.store.ListProvisionsByInstance(ctx, t.ID); perr == nil {
		for i := range ps {
			if ps[i].Status == "active" {
				v.Slices++
			}
		}
	}
	bs, err := h.store.ListBackupsByTile(ctx, t.ID)
	if err != nil {
		return v, err
	}
	for _, b := range bs {
		cv := configView{Backup: b}
		if d, _ := h.store.GetBackupDestination(ctx, b.DestinationID); d != nil {
			cv.DestName = d.Name
		}
		cv.Runs, _ = h.store.ListBackupRuns(ctx, b.ID, 10)
		v.Configs = append(v.Configs, cv)
	}
	return v, nil
}

func (h *handler) render(c echo.Context, t *repo.Tile) error {
	v, err := h.load(c, t)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, components.WithFlash(c, frag(c, v)))
}

// GET /tiles/:id/backups, the tab fragment.
func (h *handler) Panel(c echo.Context) error {
	t, err := h.loadTile(c, c.Param("id"))
	if err != nil {
		return err
	}
	return h.render(c, t)
}

// POST /tiles/:id/backups, add a schedule.
func (h *handler) Create(c echo.Context) error {
	t, err := h.loadTile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	orgID, err := backup.OrgOf(ctx, h.store, t)
	if err != nil {
		return err
	}
	// The destination is what crosses the tenant boundary: the tile is already
	// known to be this org's, the bucket credentials are not.
	if _, err := backup.ResolveDestination(ctx, h.store, orgID, c.FormValue("destination_id")); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return h.render(c, t)
	}
	b := &repo.Backup{
		ID:            uuid.New().String(),
		TileID:        sql.NullString{String: t.ID, Valid: true},
		DestinationID: c.FormValue("destination_id"),
		Kind:          repo.BackupVolume,
		ContainerMode: c.FormValue("container_mode"),
		Cron:          strings.TrimSpace(c.FormValue("cron")),
		Timezone:      strings.TrimSpace(c.FormValue("timezone")),
		KeepLatest:    atoiOr(c.FormValue("keep_latest"), 7),
		Enabled:       true,
		CreatedAt:     time.Now().UTC(),
	}
	if t.IsManaged() && c.FormValue("kind") != repo.BackupVolume {
		b.Kind = repo.BackupDump
	}
	if b.ContainerMode == "" {
		b.ContainerMode = repo.ModePause
	}
	if b.Kind == repo.BackupVolume {
		if _, err := backup.VolumeFor(t); err != nil {
			middleware.SetFlash(c, err.Error(), middleware.FlashError)
			return h.render(c, t)
		}
	}
	if err := backup.Validate(b); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return h.render(c, t)
	}
	if err := h.store.CreateBackup(ctx, b); err != nil {
		return err
	}
	h.reload(c)
	middleware.SetFlash(c, "Backup scheduled.", middleware.FlashSuccess)
	return h.render(c, t)
}

// POST /backups/:id/save
func (h *handler) Save(c echo.Context) error {
	b, t, err := h.loadBackup(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if dest := c.FormValue("destination_id"); dest != "" && dest != b.DestinationID {
		orgID, err := backup.OrgOf(ctx, h.store, t)
		if err != nil {
			return err
		}
		if _, err := backup.ResolveDestination(ctx, h.store, orgID, dest); err != nil {
			middleware.SetFlash(c, err.Error(), middleware.FlashError)
			return h.render(c, t)
		}
		b.DestinationID = dest
	}
	if m := c.FormValue("container_mode"); m != "" {
		b.ContainerMode = m
	}
	b.Cron = strings.TrimSpace(c.FormValue("cron"))
	b.Timezone = strings.TrimSpace(c.FormValue("timezone"))
	b.KeepLatest = atoiOr(c.FormValue("keep_latest"), b.KeepLatest)
	b.Enabled = c.FormValue("enabled") != ""
	if err := backup.Validate(b); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return h.render(c, t)
	}
	if err := h.store.UpdateBackup(ctx, b); err != nil {
		return err
	}
	h.reload(c)
	middleware.SetFlash(c, "Backup updated.", middleware.FlashSuccess)
	return h.render(c, t)
}

// POST /backups/:id/delete, drops the schedule. Archives already in the
// bucket are left alone; nothing here reaches into a bucket to delete history.
func (h *handler) Delete(c echo.Context) error {
	b, t, err := h.loadBackup(c)
	if err != nil {
		return err
	}
	if err := h.store.DeleteBackup(c.Request().Context(), b.ID); err != nil {
		return err
	}
	h.reload(c)
	middleware.SetFlash(c, "Backup removed. Archives already uploaded were kept.", middleware.FlashSuccess)
	return h.render(c, t)
}

// POST /backups/:id/run
func (h *handler) Run(c echo.Context) error {
	b, t, err := h.loadBackup(c)
	if err != nil {
		return err
	}
	run, err := h.svc.Run(c.Request().Context(), b.ID, "manual")
	switch {
	case err != nil:
		middleware.SetFlash(c, "Backup failed: "+err.Error(), middleware.FlashError)
	case run != nil:
		middleware.SetFlash(c, "Backup done. "+strconv.FormatInt(run.SizeBytes/1024, 10)+" KB uploaded.", middleware.FlashSuccess)
	}
	return h.render(c, t)
}

// POST /backups/:id/restore, destructive, and the confirm gate is on the
// button. The run is named by id; the object key is never taken from the form.
func (h *handler) Restore(c echo.Context) error {
	b, t, err := h.loadBackup(c)
	if err != nil {
		return err
	}
	// No RequireUnmanaged here. A restore writes data, not structure: backups
	// are not expressible in the config file at all, so no plan can revert
	// one, and the guard only made a config-managed database unrestorable
	// while still letting Run now take the backup.
	if err := h.svc.Restore(c.Request().Context(), b.ID, c.FormValue("run_id")); err != nil {
		middleware.SetFlash(c, "Restore failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Restored. The container was restarted.", middleware.FlashSuccess)
	}
	return h.render(c, t)
}

// reload re-registers cron entries after any schedule change.
func (h *handler) reload(c echo.Context) {
	if h.svc != nil {
		_ = h.svc.LoadSchedules(c.Request().Context())
	}
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}
