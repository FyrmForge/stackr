package v1

import (
	"net/http"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func toVolumeOut(v *repo.Tile) volumeOut {
	return volumeOut{ID: v.ID, Name: v.Name, MountPath: v.MountPath,
		VolumeName: v.DockerVolume(), AppID: v.AttachedTileID, Status: v.Status}
}

func (a *API) listVolumes(c echo.Context) error {
	app, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ts, err := a.store.ListTiles(c.Request().Context())
	if err != nil {
		return err
	}
	vols := repo.VolumesAttachedTo(ts, app.ID)
	out := make([]volumeOut, 0, len(vols))
	for i := range vols {
		out = append(out, toVolumeOut(&vols[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// createVolume creates a persistent volume mounted into the app and redeploys
// it so the mount takes effect.
func (a *API) createVolume(c echo.Context) error {
	app, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in volumeIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// A volume is a tile, so it is created the way every other tile is: the
	// name rules, the reserved-slug check, the duplicate check, the target
	// rule, the mount-path rule and the target's redeploy all live in the
	// tile service. This path had its own version of five of those, and its
	// target rule accepted a cron where the panel's accepted only a service.
	v := &repo.Tile{
		StackID: app.StackID, EnvironmentID: app.EnvironmentID,
		Name: in.Name, Kind: "volume", AttachedTileID: app.ID,
		MountPath: in.MountPath, VolumeName: in.VolumeName, MaxSizeMB: in.MaxSizeMB,
	}
	if _, err := a.tiles.Create(c.Request().Context(), v, nil, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toVolumeOut(v))
}

// deleteVolume removes a volume tile and redeploys its app to drop the mount.
// The underlying docker volume (its data) is preserved.
func (a *API) deleteVolume(c echo.Context) error {
	v, err := a.tile(c, c.Param("id"))
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
		a.deploys.RedeployIfRunning(ctx, v.AttachedTileID, "volume")
	}
	return c.NoContent(http.StatusNoContent)
}
