package v1

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func toVolumeOut(v *repo.Tile) volumeOut {
	return volumeOut{ID: v.ID, Name: v.Name, MountPath: v.MountPath,
		VolumeName: v.DockerVolume(), AppID: v.AttachedTileID, Status: v.Status}
}

func (a *API) listVolumes(c echo.Context) error {
	app, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	ts, err := a.store.ListTiles(c.Request().Context())
	if err != nil {
		return err
	}
	out := make([]volumeOut, 0)
	for i := range ts {
		if ts[i].IsVolume() && ts[i].AttachedTileID == app.ID {
			out = append(out, toVolumeOut(&ts[i]))
		}
	}
	return c.JSON(http.StatusOK, out)
}

// createVolume creates a persistent volume mounted into the app and redeploys
// it so the mount takes effect.
func (a *API) createVolume(c echo.Context) error {
	app, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if app.IsManaged() || app.IsVolume() {
		return echo.NewHTTPError(http.StatusBadRequest, "only services and crons can mount volumes")
	}
	if err := a.rejectManaged(c.Request().Context(), app.StackID); err != nil {
		return err
	}
	var in volumeIn
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	if !strings.HasPrefix(in.MountPath, "/") {
		return echo.NewHTTPError(http.StatusBadRequest, "mount_path must be an absolute path")
	}
	// Unchecked this lands in a bind string verbatim: "/" mounts the node's
	// root filesystem. Empty is the normal case and means the id-derived name.
	if in.VolumeName != "" && !repo.ValidVolumeName(in.VolumeName) {
		return echo.NewHTTPError(http.StatusBadRequest, "volume_name: letters, digits, _ . - only")
	}
	ctx := c.Request().Context()
	slug := repo.Slugify(in.Name)
	if repo.ReservedSlug(slug) {
		return echo.NewHTTPError(http.StatusBadRequest, "\""+slug+"\" is reserved for variable references; pick another name")
	}
	if existing, _ := a.store.GetTileBySlug(ctx, app.EnvironmentID, slug); existing != nil {
		return echo.NewHTTPError(http.StatusConflict, "a tile with that name already exists in the environment")
	}
	now := time.Now().UTC()
	if in.MaxSizeMB < 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "max_size_mb must not be negative")
	}
	v := &repo.Tile{ID: uuid.New().String(), StackID: app.StackID, EnvironmentID: app.EnvironmentID,
		Name: in.Name, Slug: slug, Kind: "volume", AttachedTileID: app.ID, MountPath: in.MountPath,
		VolumeName: in.VolumeName, MaxSizeMB: in.MaxSizeMB,
		WebhookToken: uuid.New().String(), Status: "idle", CreatedAt: now, UpdatedAt: now}
	if err := a.store.CreateTile(ctx, v); err != nil {
		return err
	}
	if _, err := a.engine.Enqueue(ctx, app, "volume"); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toVolumeOut(v))
}

// deleteVolume removes a volume tile and redeploys its app to drop the mount.
// The underlying docker volume (its data) is preserved.
func (a *API) deleteVolume(c echo.Context) error {
	v, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if !v.IsVolume() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, v.StackID); err != nil {
		return err
	}
	if err := a.teardownTile(ctx, v); err != nil {
		return err
	}
	if v.AttachedTileID != "" {
		if app, _ := a.store.GetTile(ctx, v.AttachedTileID); app != nil {
			_, _ = a.engine.Enqueue(ctx, app, "volume")
		}
	}
	return c.NoContent(http.StatusNoContent)
}
