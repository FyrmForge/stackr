package v1

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// --- destinations ---

func toDestinationOut(d *repo.BackupDestination) destinationOut {
	return destinationOut{ID: d.ID, Name: d.Name, Endpoint: d.Endpoint, Bucket: d.Bucket,
		Region: d.Region, OrgID: d.OrgID.String, Global: d.Global(), Shared: d.Shared}
}

// listDestinations returns the destinations the key's user can actually use:
// their own orgs', plus the server-wide ones an admin has shared. An unshared
// global one is simply absent, not refused: a destination carries the
// credentials the archive is written with. Admins see every one, which is what
// the admin page is. Secrets are never rendered back.
func (a *API) listDestinations(c echo.Context) error {
	ds, err := a.dests.Visible(c.Request().Context(), a.viewer(c))
	if err != nil {
		return err
	}
	out := make([]destinationOut, 0, len(ds))
	for i := range ds {
		out = append(out, toDestinationOut(&ds[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) createDestination(c echo.Context) error {
	var in destinationIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	ctx := c.Request().Context()
	// A server-wide destination is reachable from every org it is shared
	// with, so only an admin may create one. Access stays here; the trim, the
	// required fields and the bucket probe are the service's, and the panel
	// trimmed where this stored raw — which broke referencing the name from a
	// config file.
	// KindDeferred: the org arrives in the body, so the route's gate could
	// not resolve a tenancy and this check is the only one.
	if in.OrgID == "" {
		if !a.isAdmin(c) {
			return echo.NewHTTPError(http.StatusForbidden, "only admins can create a server-wide destination")
		}
	} else if err := a.requireOrgWrite(ctx, c, in.OrgID); err != nil {
		return err
	}
	d, err := a.dests.Create(ctx, service.NewDestination{
		Name: in.Name, Endpoint: in.Endpoint, Bucket: in.Bucket, Region: in.Region,
		AccessKey: in.AccessKey, SecretKey: in.SecretKey, OrgID: in.OrgID, Shared: in.Shared,
	})
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toDestinationOut(d))
}

func (a *API) deleteDestination(c echo.Context) error {
	ctx := c.Request().Context()
	d, err := a.store.GetBackupDestination(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	// 404 rather than 403 on someone else's org, so ids don't leak.
	if d == nil || (!d.Global() && !a.orgAllowed(c, d.OrgID.String)) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if d.Global() {
		if !a.isAdmin(c) {
			return echo.NewHTTPError(http.StatusForbidden, "only admins can remove a server-wide destination")
		}
	}
	if err := a.dests.Delete(ctx, d); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// --- backup configs ---

func toBackupOut(b *repo.Backup) backupOut {
	return backupOut{ID: b.ID, TileID: b.TileID.String, DestinationID: b.DestinationID,
		Kind: b.Kind, ContainerMode: b.ContainerMode, Cron: b.Cron, Timezone: b.Timezone,
		KeepLatest: b.KeepLatest, Enabled: b.Enabled}
}

// loadBackup loads a backup and the tile it hangs off, which is where its
// tenancy comes from. The route's gate resolves the same walk (KindBackup in
// service/tenancy.go) and checks the level; this is the row itself.
//
// The panel's own backup has no tile: it belongs to the installation, and the
// gate answers for it through service.ErrServerOwned. Here it just means
// there is no tile to return.
func (a *API) loadBackup(c echo.Context) (*repo.Backup, *repo.Tile, error) {
	b, err := a.store.GetBackup(c.Request().Context(), c.Param("id"))
	if err != nil || b == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if b.Kind == repo.BackupStackr {
		return b, nil, nil
	}
	t, err := a.tile(c, b.TileID.String)
	if err != nil {
		return nil, nil, err
	}
	return b, t, nil
}

func (a *API) listBackups(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	bs, err := a.store.ListBackupsByTile(c.Request().Context(), t.ID)
	if err != nil {
		return err
	}
	out := make([]backupOut, 0, len(bs))
	for i := range bs {
		out = append(out, toBackupOut(&bs[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) createBackup(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in backupIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// Every rule is the service's. This path accepted a dump for a tile that
	// is not a database, defaulted keep to 0 (which means keep nothing), and
	// did not gate a write to a key the config file owns.
	b, err := a.schedules.Create(c.Request().Context(), t, service.ScheduleSpec{
		Dest: in.DestinationID, Kind: in.Kind, Mode: in.ContainerMode,
		Cron: &in.Cron, Timezone: &in.Timezone, Keep: in.KeepLatest, Enabled: in.Enabled,
	}, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toBackupOut(b))
}

// resolveDestinationRef accepts either a destination id or the reference form
// the config file uses, ${{ org.backups.NAME }} / ${{ stackr.backups.NAME }},
// and returns the id. One spelling everywhere: a script that reads a name out
// of a config file should not have to look the id up first.
func (a *API) resolveDestinationRef(ctx context.Context, orgID, ref string) (string, error) {
	if scope, name, ok := backup.ParseRef(ref); ok {
		d, err := backup.ResolveNamed(ctx, a.store, orgID, scope, name)
		if err != nil {
			return "", err
		}
		return d.ID, nil
	}
	if _, err := backup.ResolveDestination(ctx, a.store, orgID, ref); err != nil {
		return "", err
	}
	return ref, nil
}

func (a *API) patchBackup(c echo.Context) error {
	b, t, err := a.loadBackup(c)
	if err != nil {
		return err
	}
	var in backupPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// Cron and timezone are pointers on the patch now, so clearing a timezone
	// is expressible. It was not: "apply only if non-empty" meant a timezone,
	// once set, could never be removed over the API or the CLI.
	if err := a.schedules.Update(c.Request().Context(), b, t, service.ScheduleSpec{
		Dest: in.DestinationID, Mode: in.ContainerMode,
		Cron: in.Cron, Timezone: in.Timezone, Keep: in.KeepLatest, Enabled: in.Enabled,
	}, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, toBackupOut(b))
}

func (a *API) deleteBackup(c echo.Context) error {
	b, _, err := a.loadBackup(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.store.DeleteBackup(ctx, b.ID); err != nil {
		return err
	}
	a.sched.ReloadBackups(ctx)
	return c.NoContent(http.StatusNoContent)
}

// --- runs ---

func toRunOut(r *repo.BackupRun) backupRunOut {
	out := backupRunOut{ID: r.ID, BackupID: r.BackupID, Trigger: r.Trigger, Status: r.Status,
		ObjectKey: r.ObjectKey, SizeBytes: r.SizeBytes, Error: r.Error,
		CreatedAt: r.CreatedAt.Format(time.RFC3339)}
	if r.FinishedAt.Valid {
		out.FinishedAt = r.FinishedAt.Time.Format(time.RFC3339)
	}
	return out
}

func (a *API) listBackupRuns(c echo.Context) error {
	b, _, err := a.loadBackup(c)
	if err != nil {
		return err
	}
	rs, err := a.store.ListBackupRuns(c.Request().Context(), b.ID, 50)
	if err != nil {
		return err
	}
	out := make([]backupRunOut, 0, len(rs))
	for i := range rs {
		out = append(out, toRunOut(&rs[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) runBackup(c echo.Context) error {
	b, _, err := a.loadBackup(c)
	if err != nil {
		return err
	}
	if a.backups == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "backups are not available")
	}
	// Queued, not run: the row comes back "queued" and the runs list follows it.
	run, err := a.backups.Start(c.Request().Context(), b.ID, "manual")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusAccepted, toRunOut(run))
}

// restoreBackup puts one recorded run back. The archive is named by run id,
// never by object key, a key off the wire would read any object in the
// bucket, which on a shared destination means another org's backups.
func (a *API) restoreBackup(c echo.Context) error {
	b, _, err := a.loadBackup(c)
	if err != nil {
		return err
	}
	var in restoreIn
	if err := c.Bind(&in); err != nil || in.RunID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "run_id required")
	}
	// Checked here as well as in the service: this is the boundary the id
	// arrives at, and a run from another schedule is another org's archive.
	run, err := a.store.GetBackupRun(c.Request().Context(), in.RunID)
	if err != nil || run == nil || run.BackupID != b.ID {
		return echo.NewHTTPError(http.StatusNotFound, "backup run not found")
	}
	if a.backups == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "backups are not available")
	}
	ctx := c.Request().Context()
	if _, err := a.backups.StartRestore(ctx, b.ID, in.RunID); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return a.restoreStatus(c, b.ID)
}

// getRestore is the newest restore of a backup. The CLI polls it to wait for
// a restore it queued.
func (a *API) getRestore(c echo.Context) error {
	b, _, err := a.loadBackup(c)
	if err != nil {
		return err
	}
	if a.backups == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "backups are not available")
	}
	return a.restoreStatus(c, b.ID)
}

func (a *API) restoreStatus(c echo.Context, backupID string) error {
	w, err := a.backups.LatestRestore(c.Request().Context(), backupID)
	if err != nil {
		return err
	}
	if w == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no restore for this backup")
	}
	out := restoreOut{ID: w.ID, Status: w.Status, Step: w.Step, Error: w.Error,
		CreatedAt: w.CreatedAt.Format(time.RFC3339)}
	if w.FinishedAt.Valid {
		out.FinishedAt = w.FinishedAt.Time.Format(time.RFC3339)
	}
	return c.JSON(http.StatusOK, out)
}

// patchDestination flips the shared toggle on a server-wide destination.
// Nothing else is patchable: endpoint, bucket and credentials are what the
// archives already written were written with, so changing them in place would
// silently orphan them.
//
// Turning it off while an org still schedules against it is refused, and the
// error names the tiles: the alternative is a backup that starts failing at 3am
// for a reason nobody can see from the panel.
func (a *API) patchDestination(c echo.Context) error {
	ctx := c.Request().Context()
	d, err := a.store.GetBackupDestination(ctx, c.Param("id"))
	if err != nil || d == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if !d.Global() {
		return echo.NewHTTPError(http.StatusBadRequest, "sharing applies to server-wide destinations only")
	}
	if !a.isAdmin(c) {
		return echo.NewHTTPError(http.StatusForbidden, "only admins can change a server-wide destination")
	}
	var in destinationPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.Shared != nil && !*in.Shared && d.Shared {
		if users, _ := a.dests.Users(ctx, d.ID); len(users) > 0 {
			return echo.NewHTTPError(http.StatusConflict,
				"still used by "+strings.Join(users, ", "))
		}
	}
	if in.Shared != nil {
		d.Shared = *in.Shared
	}
	if err := a.store.UpdateBackupDestination(ctx, d); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toDestinationOut(d))
}
