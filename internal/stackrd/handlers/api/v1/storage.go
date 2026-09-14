package v1

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Server-scoped storage (§2.7). Admin-shaped: keys need storage:* scopes.
// Attachments ride the tile PATCH (`storage` lines), not endpoints here.

type storageIn struct {
	Name     string `json:"name" required:"true"`
	Backend  string `json:"backend" required:"true" description:"nfs | smb | local"`
	Address  string `json:"address" description:"host, for nfs/smb"`
	Export   string `json:"export" description:"/export (nfs) | share (smb) | absolute host path (local)"`
	Username string `json:"username" description:"smb"`
	Password string `json:"password" description:"smb; encrypted at rest (docker volume metadata keeps a plaintext copy)"`
	Opts     string `json:"opts" description:"extra mount opts"`
}

type storagePathIn struct {
	ID       string `path:"id"` // storage id
	Name     string `json:"name" required:"true"`
	Subpath  string `json:"subpath" description:"relative to the share/pool root; empty = root"`
	ForcedRO bool   `json:"forced_ro" description:"every attachment of this sub-path is read-only"`
}

type storagePathOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Subpath  string `json:"subpath"`
	ForcedRO bool   `json:"forced_ro"`
	Volume   string `json:"volume" description:"backing docker volume name"`
}

type storageOut struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Slug      string           `json:"slug"`
	Backend   string           `json:"backend"`
	Address   string           `json:"address"`
	Export    string           `json:"export"`
	Status    string           `json:"status"`
	StatusMsg string           `json:"status_msg"`
	Paths     []storagePathOut `json:"paths"`
}

func (a *API) storageOut(c echo.Context, st *repo.Storage) storageOut {
	out := storageOut{ID: st.ID, Name: st.Name, Slug: st.Slug, Backend: st.Backend,
		Address: st.Address, Export: st.Export, Status: st.Status, StatusMsg: st.StatusMsg}
	paths, _ := a.store.ListStoragePaths(c.Request().Context(), st.ID)
	for _, p := range paths {
		out.Paths = append(out.Paths, storagePathOut{ID: p.ID, Name: p.Name, Subpath: p.Subpath,
			ForcedRO: p.ForcedRO, Volume: repo.StorageVolume(p.ID)})
	}
	return out
}

func (a *API) listStorage(c echo.Context) error {
	sts, err := a.store.ListStorage(c.Request().Context())
	if err != nil {
		return err
	}
	out := make([]storageOut, 0, len(sts))
	for i := range sts {
		out = append(out, a.storageOut(c, &sts[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) createStorage(c echo.Context) error {
	ctx := c.Request().Context()
	var in storageIn
	if err := c.Bind(&in); err != nil || in.Name == "" || !storagetiles.ValidBackend(in.Backend) {
		return echo.NewHTTPError(http.StatusBadRequest, "name and a backend (nfs, smb or local) required")
	}
	if in.Backend == "local" && !strings.HasPrefix(in.Export, "/") {
		return echo.NewHTTPError(http.StatusBadRequest, "a local pool needs an absolute host path")
	}
	if in.Backend != "local" && in.Address == "" {
		return echo.NewHTTPError(http.StatusBadRequest, in.Backend+" storage needs an address")
	}
	slug := repo.Slugify(in.Name)
	if existing, _ := a.store.GetStorageBySlug(ctx, slug); existing != nil {
		return echo.NewHTTPError(http.StatusConflict, "storage "+slug+" already exists")
	}
	st := &repo.Storage{ID: uuid.New().String(), ServerID: "local", Name: in.Name, Slug: slug,
		Backend: in.Backend, Address: in.Address, Export: in.Export, Username: in.Username,
		Password: in.Password, Opts: in.Opts, Status: "unknown", CreatedAt: time.Now().UTC()}
	if err := a.store.CreateStorage(ctx, st); err != nil {
		return err
	}
	// Probe on create, a broken share should fail here, not at first deploy.
	// On the server's own node: probing the manager proves nothing about the
	// machine that will do the mounting.
	node, nerr := a.clus.NodeOfStorage(ctx, st)
	probe := repo.StoragePath{ID: uuid.New().String(), StorageID: st.ID}
	switch {
	case nerr != nil:
		st.Status, st.StatusMsg = "error", nerr.Error()
	default:
		if err := storagetiles.Probe(ctx, a.clus, node, st, &probe); err != nil {
			st.Status, st.StatusMsg = "error", err.Error()
		} else {
			st.Status, st.StatusMsg = "ok", ""
		}
		_ = a.clus.RemoveVolume(ctx, node, repo.StorageVolume(probe.ID))
	}
	if err := a.store.UpdateStorage(ctx, st); err != nil {
		slog.Error("storage probe result not saved", "storage", st.ID, "status", st.Status, "error", err)
	}
	return c.JSON(http.StatusCreated, a.storageOut(c, st))
}

func (a *API) deleteStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := a.store.GetStorage(ctx, c.Param("id"))
	if err != nil || st == nil {
		return echo.NewHTTPError(http.StatusNotFound, "storage not found")
	}
	tiles, _ := a.store.ListTiles(ctx)
	for i := range tiles {
		for _, l := range strings.Split(tiles[i].Storage, "\n") {
			if slug, _, _, _, err := storagetiles.ParseAttachment(strings.TrimSpace(l)); err == nil && slug == st.Slug {
				return echo.NewHTTPError(http.StatusConflict, "still attached to "+tiles[i].Name+"; detach first")
			}
		}
	}
	paths, _ := a.store.ListStoragePaths(ctx, st.ID)
	if node, err := a.clus.NodeOfStorage(ctx, st); err == nil {
		for i := range paths {
			_ = a.clus.RemoveVolume(ctx, node, repo.StorageVolume(paths[i].ID))
		}
	}
	if err := a.store.DeleteStorage(ctx, st.ID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, struct{}{})
}

func (a *API) createStoragePath(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := a.store.GetStorage(ctx, c.Param("id"))
	if err != nil || st == nil {
		return echo.NewHTTPError(http.StatusNotFound, "storage not found")
	}
	var in storagePathIn
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "sub-path name required")
	}
	sub := strings.Trim(in.Subpath, "/")
	if strings.Contains(sub, "..") {
		return echo.NewHTTPError(http.StatusBadRequest, "sub-path must stay inside the share")
	}
	p := &repo.StoragePath{ID: uuid.New().String(), StorageID: st.ID, Name: repo.Slugify(in.Name),
		Subpath: sub, ForcedRO: in.ForcedRO, CreatedAt: time.Now().UTC()}
	if err := a.store.CreateStoragePath(ctx, p); err != nil {
		return err
	}
	node, err := a.clus.NodeOfStorage(ctx, st)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	if err := storagetiles.Probe(ctx, a.clus, node, st, p); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "sub-path declared but mounting it failed: "+err.Error())
	}
	return c.JSON(http.StatusCreated, storagePathOut{ID: p.ID, Name: p.Name, Subpath: p.Subpath,
		ForcedRO: p.ForcedRO, Volume: repo.StorageVolume(p.ID)})
}

func (a *API) deleteStoragePath(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := a.store.GetStoragePath(ctx, c.Param("id"))
	if err != nil || p == nil {
		return echo.NewHTTPError(http.StatusNotFound, "sub-path not found")
	}
	st, _ := a.store.GetStorage(ctx, p.StorageID)
	if st != nil {
		tiles, _ := a.store.ListTiles(ctx)
		for i := range tiles {
			for _, l := range strings.Split(tiles[i].Storage, "\n") {
				if slug, name, _, _, err := storagetiles.ParseAttachment(strings.TrimSpace(l)); err == nil && slug == st.Slug && name == p.Name {
					return echo.NewHTTPError(http.StatusConflict, "still attached to "+tiles[i].Name+"; detach first")
				}
			}
		}
	}
	if node, err := a.clus.NodeOfStorage(ctx, st); err == nil {
		_ = a.clus.RemoveVolume(ctx, node, repo.StorageVolume(p.ID))
	}
	if err := a.store.DeleteStoragePath(ctx, p.ID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, struct{}{})
}
