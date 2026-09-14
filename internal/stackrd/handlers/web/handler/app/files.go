package app

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/filebrowse"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// File routes for a volume tile's Tree tab, the same shared browser the db
// data volumes use, over the tile's backing docker volume.

func (h *handler) loadVolumeTile(c echo.Context) (*repo.Tile, filebrowse.VolumeFS, error) {
	a, err := h.load(c)
	if err != nil {
		return nil, filebrowse.VolumeFS{}, err
	}
	if !a.IsVolume() {
		return nil, filebrowse.VolumeFS{}, echo.NewHTTPError(http.StatusNotFound, "not a volume")
	}
	// The attached service's node, not the volume tile's own column: the
	// volume is a mount on that service. An error names the tile rather than
	// browsing the wrong machine's disk.
	node, err := h.clus.NodeOf(c.Request().Context(), a)
	if err != nil {
		return nil, filebrowse.VolumeFS{}, echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return a, filebrowse.VolumeFS{Cluster: h.clus, Node: node, Vol: a.DockerVolume()}, nil
}

func volumeFilesBase(a *repo.Tile) string { return "/apps/" + a.ID + "/volume/files" }

// GET /apps/:id/volume/files?path=
func (h *handler) VolumeFiles(c echo.Context) error {
	a, fs, err := h.loadVolumeTile(c)
	if err != nil {
		return err
	}
	return filebrowse.Browse(c, fs, volumeFilesBase(a))
}

// GET /apps/:id/volume/files/download?path=
func (h *handler) VolumeFileDownload(c echo.Context) error {
	_, fs, err := h.loadVolumeTile(c)
	if err != nil {
		return err
	}
	return filebrowse.Download(c, fs)
}

// POST /apps/:id/volume/files/upload
func (h *handler) VolumeFileUpload(c echo.Context) error {
	a, fs, err := h.loadVolumeTile(c)
	if err != nil {
		return err
	}
	return filebrowse.Upload(c, fs, volumeFilesBase(a))
}

// POST /apps/:id/volume/files/delete
func (h *handler) VolumeFileDelete(c echo.Context) error {
	a, fs, err := h.loadVolumeTile(c)
	if err != nil {
		return err
	}
	return filebrowse.Delete(c, fs, volumeFilesBase(a))
}
