// Package filebrowse is the shared file browser: one fragment UI and one set
// of route bodies over any store that can list/read/write/delete by path.
// Backends today: docker volumes (db data volumes, service volume tiles) and
// s3 buckets.
package filebrowse

import (
	"context"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
)

// Entry is one row of a listing.
type Entry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

// Backend is what a store must speak for the shared handlers/templ.
type Backend interface {
	List(ctx context.Context, dir string) ([]Entry, error)
	Read(ctx context.Context, file string) (io.ReadCloser, error)
	Write(ctx context.Context, file string, src io.Reader) error
	Delete(ctx context.Context, p string, dir bool) error
}

// CleanDir confines a user-supplied dir to a /-rooted path with no "..".
func CleanDir(p string) string { return path.Clean("/" + p) }

// Browse renders the listing fragment for ?path=.
func Browse(c echo.Context, fs Backend, base string) error {
	dir := CleanDir(c.QueryParam("path"))
	entries, err := fs.List(c.Request().Context(), dir)
	if err != nil {
		return respond.HTML(c, http.StatusOK, ErrorFrag(base, err.Error()))
	}
	return respond.HTML(c, http.StatusOK, Browser(c, base, dir, entries, stackrmw.CanWriteHere(c)))
}

// Download streams ?path= as an attachment.
func Download(c echo.Context, fs Backend) error {
	file := CleanDir(c.QueryParam("path"))
	if file == "/" {
		return echo.NewHTTPError(http.StatusBadRequest, "path required")
	}
	src, err := fs.Read(c.Request().Context(), file)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	defer func() { _ = src.Close() }()
	c.Response().Header().Set("Content-Disposition", `attachment; filename="`+path.Base(file)+`"`)
	return c.Stream(http.StatusOK, "application/octet-stream", src)
}

// Upload stores the posted file into the dir in form value "path", then
// re-renders the listing there.
func Upload(c echo.Context, fs Backend, base string) error {
	dir := CleanDir(c.FormValue("path"))
	fh, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "file required")
	}
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if err := fs.Write(c.Request().Context(), path.Join(dir, path.Base(fh.Filename)), src); err != nil {
		return respond.HTML(c, http.StatusOK, ErrorFrag(base, err.Error()))
	}
	return rebrowse(c, fs, base, dir)
}

// Delete removes form value "target" (dir flag in "dir"), then re-renders the
// listing at "path".
func Delete(c echo.Context, fs Backend, base string) error {
	dir := CleanDir(c.FormValue("path"))
	target := CleanDir(c.FormValue("target"))
	if target == "/" {
		return echo.NewHTTPError(http.StatusBadRequest, "target required")
	}
	if err := fs.Delete(c.Request().Context(), target, c.FormValue("dir") == "1"); err != nil {
		return respond.HTML(c, http.StatusOK, ErrorFrag(base, err.Error()))
	}
	return rebrowse(c, fs, base, dir)
}

func rebrowse(c echo.Context, fs Backend, base, dir string) error {
	entries, err := fs.List(c.Request().Context(), dir)
	if err != nil {
		return respond.HTML(c, http.StatusOK, ErrorFrag(base, err.Error()))
	}
	return respond.HTML(c, http.StatusOK, Browser(c, base, dir, entries, stackrmw.CanWriteHere(c)))
}

// crumb is one breadcrumb segment.
type crumb struct {
	Name string
	Path string
}

func crumbs(dir string) []crumb {
	cs := []crumb{{Name: "root", Path: "/"}}
	acc := ""
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if seg == "" {
			continue
		}
		acc += "/" + seg
		cs = append(cs, crumb{Name: seg, Path: acc})
	}
	return cs
}

// VolumeFS browses a docker volume via the throwaway-container ops, on
// whichever node holds it.
//
// Node is the tile's home node and is the whole multi-node story here: a
// volume is a directory on one machine's disk, so browsing one on the manager
// while the tile runs on a worker shows the manager's copy, the wrong files,
// with no error to say so. Empty means "here", which is every single-node
// install.
type VolumeFS struct {
	Cluster *cluster.Cluster
	Node    string
	Vol     string
}

func (v VolumeFS) List(ctx context.Context, dir string) ([]Entry, error) {
	es, err := v.Cluster.ListVolumeFiles(ctx, v.Node, v.Vol, dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(es))
	for i, e := range es {
		out[i] = Entry{Name: e.Name, Dir: e.Dir, Size: e.Size, ModTime: e.ModTime}
	}
	return out, nil
}

func (v VolumeFS) Read(ctx context.Context, file string) (io.ReadCloser, error) {
	src, wait, err := v.Cluster.ReadVolumeFile(ctx, v.Node, v.Vol, file)
	if err != nil {
		return nil, err
	}
	return waitCloser{src, wait}, nil
}

func (v VolumeFS) Write(ctx context.Context, file string, src io.Reader) error {
	return v.Cluster.WriteVolumeFile(ctx, v.Node, v.Vol, file, src)
}

func (v VolumeFS) Delete(ctx context.Context, p string, dir bool) error {
	return v.Cluster.DeleteVolumeFile(ctx, v.Node, v.Vol, p) // rm -rf handles both
}

// waitCloser reaps the streaming docker process on Close.
type waitCloser struct {
	io.ReadCloser
	wait func() error
}

func (w waitCloser) Close() error {
	_ = w.ReadCloser.Close()
	return w.wait()
}
