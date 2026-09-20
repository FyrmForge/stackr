// Package backups is the per-tile Backups tab: the schedules on one tile,
// their run history, and the buttons that run or restore them.
//
// It is one fragment rather than two tab bodies because both tile kinds that
// can be backed up (database tiles and volume tiles) render through
// different panel packages (db and app). The tab in each is a div that fetches
// this.
package backups

import (
	"net/http"
	"strconv"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store repo.Store
	svc   *backup.Service
	// sched re-registers the cron and backup tables after a write that
	// changes or cascades their rows.
	sched *scheduler.Service
	// schedules owns the rules a schedule is created and edited under;
	// dests owns which destinations this tile may be pointed at.
	schedules *service.BackupScheduleService
	dests     *service.BackupDestinationService
	// tiles owns the tile a backup is taken of.
	tiles  *service.TileService
	slices *service.SliceService
}

// WithBackups attaches the two backup services.
func (h *handler) WithBackups(sch *service.BackupScheduleService, d *service.BackupDestinationService) *handler {
	h.schedules, h.dests = sch, d
	return h
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
	Restore  *repo.WorkItem // the newest restore, nil when there has been none
}

// restoreShown is how long a failed restore stays on the tab.
const restoreShown = 24 * time.Hour

// RestoreFailed reports a restore that failed recently enough to still say so.
func (cv configView) RestoreFailed() bool {
	r := cv.Restore
	return r != nil && r.Status == "error" && r.FinishedAt.Valid && time.Since(r.FinishedAt.Time) < restoreShown
}

// Active reports work still going, which is when the history polls.
func (cv configView) Active() bool {
	for _, r := range cv.Runs {
		if r.Status == "queued" || r.Status == "running" {
			return true
		}
	}
	return cv.Restore != nil && !cv.Restore.Done()
}

// loadTile resolves the tile in the URL and checks org membership. Every route
// in this file starts here, a backup resolved by id alone, with no walk back
// to the tile's org, is the hole the removed feature shipped with.
func (h *handler) loadTile(c echo.Context, id string) (*repo.Tile, error) {
	t, err := h.tiles.Get(c.Request().Context(), id)
	if err != nil {
		return nil, stackrmw.HTTP(err)
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
	// An unshared server-wide destination is not offered here: it carries the
	// credentials the archive is written with, and the admin has not handed
	// it out. One rule, in the destination service, where the admin page, the
	// org page and the API each had their own.
	dests, err := h.dests.Visible(ctx, service.Viewer{Orgs: map[string]bool{orgID: true}})
	if err != nil {
		return v, err
	}
	v.Dests = append(v.Dests, dests...)
	// A dump covers the instance's own database only. Slices are separate
	// databases in the same server, and per-slice backups are phase 4 of
	// docs/features/shared-infra.md, so say so rather than let a 400-byte
	// artifact read as a backup of everything.
	if ps, perr := h.slices.ForInstance(ctx, t.ID); perr == nil {
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
		v.Configs = append(v.Configs, h.configView(c, b))
	}
	return v, nil
}

func (h *handler) configView(c echo.Context, b repo.Backup) configView {
	ctx := c.Request().Context()
	cv := configView{Backup: b}
	if d, _ := h.store.GetBackupDestination(ctx, b.DestinationID); d != nil {
		cv.DestName = d.Name
	}
	cv.Runs, _ = h.store.ListBackupRuns(ctx, b.ID, 10)
	cv.Restore, _ = h.svc.LatestRestore(ctx, b.ID)
	return cv
}

// GET /backups/:id/history, the run list alone. Polled while work is going,
// so a schedule being edited above it is not wiped.
func (h *handler) History(c echo.Context) error {
	b, _, err := h.loadBackup(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, history(c, h.configView(c, *b)))
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
	// The destination resolution, the kind derivation, the volume guard, the
	// mode and keep defaults, the validator and the schedule reload are all
	// the service's. This path and the API's had a different answer for four
	// of those six.
	keep := atoiOr(c.FormValue("keep_latest"), service.DefaultKeep)
	cron, tz := c.FormValue("cron"), c.FormValue("timezone")
	if _, err := h.schedules.Create(c.Request().Context(), t, service.ScheduleSpec{
		Dest: c.FormValue("destination_id"),
		Kind: c.FormValue("kind"),
		Mode: c.FormValue("container_mode"),
		Cron: &cron, Timezone: &tz, Keep: &keep,
	}, stackrmw.WebActor(c)); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return h.render(c, t)
	}
	middleware.SetFlash(c, "Backup scheduled.", middleware.FlashSuccess)
	return h.render(c, t)
}

// POST /backups/:id/save
func (h *handler) Save(c echo.Context) error {
	b, t, err := h.loadBackup(c)
	if err != nil {
		return err
	}
	// Cron and timezone come from the form either way, so both are clearable
	// here; the API and the CLI could not clear a timezone at all, which was
	// an artefact of "patch only non-empty" rather than a rule.
	keep := atoiOr(c.FormValue("keep_latest"), b.KeepLatest)
	cron, tz := c.FormValue("cron"), c.FormValue("timezone")
	enabled := c.FormValue("enabled") != ""
	if err := h.schedules.Update(c.Request().Context(), b, t, service.ScheduleSpec{
		Dest: c.FormValue("destination_id"),
		Mode: c.FormValue("container_mode"),
		Cron: &cron, Timezone: &tz, Keep: &keep, Enabled: &enabled,
	}, stackrmw.WebActor(c)); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return h.render(c, t)
	}
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
	if err := h.schedules.Delete(c.Request().Context(), b); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Backup removed. Archives already uploaded were kept.", middleware.FlashSuccess)
	return h.render(c, t)
}

// POST /backups/:id/run
func (h *handler) Run(c echo.Context) error {
	b, t, err := h.loadBackup(c)
	if err != nil {
		return err
	}
	if _, err := h.svc.Start(c.Request().Context(), b.ID, "manual"); err != nil {
		middleware.SetFlash(c, "Backup failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Backup started.", middleware.FlashSuccess)
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
	if _, err := h.svc.StartRestore(c.Request().Context(), b.ID, c.FormValue("run_id")); err != nil {
		middleware.SetFlash(c, "Restore failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Restore started.", middleware.FlashSuccess)
	}
	return h.render(c, t)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

// WithScheduler gives the handler the schedule reloader.
func (h *handler) WithScheduler(s *scheduler.Service) *handler { h.sched = s; return h }

// WithTiles gives the page the tile service.
func (h *handler) WithTiles(t *service.TileService) *handler { h.tiles = t; return h }

// WithSlices gives the page the provision service.
func (h *handler) WithSlices(v *service.SliceService) *handler { h.slices = v; return h }
