package db

import (
	"context"
	"io"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/filebrowse"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// File routes for the two db-side stores: a database's data volume and an s3
// bucket. The browser UI and route bodies live in filebrowse; this file only
// resolves auth + the backend.

// loadVolume is load() plus the volume identity for file routes.
func (h *handler) loadVolume(c echo.Context) (*repo.Tile, filebrowse.VolumeFS, error) {
	d, err := h.load(c)
	if err != nil {
		return nil, filebrowse.VolumeFS{}, err
	}
	if !d.IsManaged() {
		return nil, filebrowse.VolumeFS{}, echo.NewHTTPError(http.StatusNotFound, "not a database")
	}
	// Through the cluster, not d.HomeNode raw: an instance with no home node
	// yet gets a named error instead of a browse of the wrong disk.
	node, err := h.clus.NodeOf(c.Request().Context(), d)
	if err != nil {
		return nil, filebrowse.VolumeFS{}, echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return d, filebrowse.VolumeFS{Cluster: h.clus, Node: node, Vol: managedtiles.VolumeName(d)}, nil
}

func volumeFilesBase(d *repo.Tile) string { return "/dbs/" + d.ID + "/volume/files" }

// GET /dbs/:id/volume/files?path=
func (h *handler) VolumeFiles(c echo.Context) error {
	d, fs, err := h.loadVolume(c)
	if err != nil {
		return err
	}
	return filebrowse.Browse(c, fs, volumeFilesBase(d))
}

// GET /dbs/:id/volume/files/download?path=
func (h *handler) VolumeFileDownload(c echo.Context) error {
	_, fs, err := h.loadVolume(c)
	if err != nil {
		return err
	}
	return filebrowse.Download(c, fs)
}

// POST /dbs/:id/volume/files/upload
func (h *handler) VolumeFileUpload(c echo.Context) error {
	d, fs, err := h.loadVolume(c)
	if err != nil {
		return err
	}
	return filebrowse.Upload(c, fs, volumeFilesBase(d))
}

// POST /dbs/:id/volume/files/delete
func (h *handler) VolumeFileDelete(c echo.Context) error {
	d, fs, err := h.loadVolume(c)
	if err != nil {
		return err
	}
	return filebrowse.Delete(c, fs, volumeFilesBase(d))
}

// --- s3 bucket backend ---

type bucketFS struct {
	h        *handler
	instance *repo.Tile
	bucket   string
}

func (b bucketFS) List(ctx context.Context, dir string) ([]filebrowse.Entry, error) {
	os, err := b.h.dbs.S3List(ctx, b.instance, b.bucket, dir)
	if err != nil {
		return nil, err
	}
	out := make([]filebrowse.Entry, len(os))
	for i, o := range os {
		out[i] = filebrowse.Entry{Name: o.Name, Dir: o.Dir, Size: o.Size, ModTime: o.ModTime}
	}
	return out, nil
}

func (b bucketFS) Read(ctx context.Context, file string) (io.ReadCloser, error) {
	return b.h.dbs.S3Get(ctx, b.instance, b.bucket, file)
}

func (b bucketFS) Write(ctx context.Context, file string, src io.Reader) error {
	return b.h.dbs.S3Put(ctx, b.instance, b.bucket, file, src)
}

func (b bucketFS) Delete(ctx context.Context, p string, dir bool) error {
	return b.h.dbs.S3Delete(ctx, b.instance, b.bucket, p, dir)
}

// loadBucket is load() plus proof the named bucket was provisioned on this
// instance, the bucket param is user input, not a trusted id.
func (h *handler) loadBucket(c echo.Context) (*repo.Tile, bucketFS, bool, error) {
	d, err := h.load(c)
	if err != nil {
		return nil, bucketFS{}, false, err
	}
	if !managedtiles.Engines[d.Engine].FileBrowser {
		return nil, bucketFS{}, false, echo.NewHTTPError(http.StatusNotFound, "this engine has no file browser")
	}
	bucket := c.Param("bucket")
	ps, err := h.slices.ForInstance(c.Request().Context(), d.ID)
	if err != nil {
		return nil, bucketFS{}, false, err
	}
	for _, p := range ps {
		if p.DBName == bucket {
			return d, bucketFS{h: h, instance: d, bucket: bucket}, p.Public, nil
		}
	}
	return nil, bucketFS{}, false, echo.NewHTTPError(http.StatusNotFound, "bucket not found")
}

func bucketFilesBase(d *repo.Tile, bucket string) string {
	return "/dbs/" + d.ID + "/buckets/" + bucket + "/files"
}

// GET /dbs/:id/buckets/:bucket/panel, the drawer behind a bucket slice card.
func (h *handler) BucketPanel(c echo.Context) error {
	d, fs, public, err := h.loadBucket(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, bucketPanel(d, fs.bucket, public, c.QueryParam("tab")))
}

// GET /dbs/:id/buckets/:bucket/files?path=
func (h *handler) BucketFiles(c echo.Context) error {
	d, fs, _, err := h.loadBucket(c)
	if err != nil {
		return err
	}
	return filebrowse.Browse(c, fs, bucketFilesBase(d, fs.bucket))
}

// GET /dbs/:id/buckets/:bucket/files/download?path=
func (h *handler) BucketFileDownload(c echo.Context) error {
	_, fs, _, err := h.loadBucket(c)
	if err != nil {
		return err
	}
	return filebrowse.Download(c, fs)
}

// POST /dbs/:id/buckets/:bucket/files/upload
func (h *handler) BucketFileUpload(c echo.Context) error {
	d, fs, _, err := h.loadBucket(c)
	if err != nil {
		return err
	}
	return filebrowse.Upload(c, fs, bucketFilesBase(d, fs.bucket))
}

// POST /dbs/:id/buckets/:bucket/files/delete
func (h *handler) BucketFileDelete(c echo.Context) error {
	d, fs, _, err := h.loadBucket(c)
	if err != nil {
		return err
	}
	return filebrowse.Delete(c, fs, bucketFilesBase(d, fs.bucket))
}
