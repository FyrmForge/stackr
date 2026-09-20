package v1

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
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
	Org      string `json:"org" description:"org slug: makes an org network share (nfs/smb) its tiles mount as ${{ org.storage.NAME }}"`
	// ServerID names the node a pool lives on. Defaults to the manager,
	// which is what this route used to hard-code with no way to say
	// otherwise, so every pool a CLI created landed there.
	ServerID string `json:"server_id" description:"node id the pool is mounted on; defaults to the manager"`
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
	Org       string           `json:"org,omitempty" description:"org slug, set on an org share"`
	Paths     []storagePathOut `json:"paths"`
}

func (a *API) storageOut(c echo.Context, st *repo.Storage) storageOut {
	out := storageOut{ID: st.ID, Name: st.Name, Slug: st.Slug, Backend: st.Backend,
		Address: st.Address, Export: st.Export, Status: st.Status, StatusMsg: st.StatusMsg}
	if st.OrgID != "" {
		if org, _ := a.orgs.Get(c.Request().Context(), st.OrgID); org != nil {
			out.Org = org.Slug
		}
	}
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
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	spec := service.StorageSpec{
		Name: in.Name, Backend: in.Backend, Address: in.Address, Export: in.Export,
		Username: in.Username, Password: in.Password, Opts: in.Opts,
		// The node the pool lives on, from the caller. This used to be
		// hard-coded "local", which is why every pool a CLI created probed
		// and mounted on the manager rather than on the machine meant for it.
		ServerID: in.ServerID,
	}
	if spec.ServerID == "" && in.Org == "" {
		spec.ServerID = "local"
	}
	if in.Org != "" {
		org, err := a.orgShareOwner(ctx, in.Org)
		if err != nil {
			return err
		}
		spec.OrgID = org.ID
	}
	st, err := a.storage.Create(ctx, spec)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, a.storageOut(c, st))
}

func (a *API) deleteStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := a.store.GetStorage(ctx, c.Param("id"))
	if err != nil || st == nil {
		return echo.NewHTTPError(http.StatusNotFound, "storage not found")
	}
	if st.OrgID != "" {
		// Access first: an org share is the org's to remove. The consumer
		// scan and the volume cleanup are the service's, for both kinds.
		org, oerr := a.orgs.Get(ctx, st.OrgID)
		if oerr != nil {
			return stackrmw.HTTP(oerr)
		}
		if _, oerr := a.orgShareOwner(ctx, org.Slug); oerr != nil {
			return oerr
		}
	}
	if err := a.storage.Delete(ctx, st); err != nil {
		return stackrmw.HTTP(err)
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
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	p, err := a.storage.DeclarePath(ctx, st, in.Name, in.Subpath, in.ForcedRO)
	if p == nil {
		return stackrmw.HTTP(err)
	}
	if err != nil {
		// Declared, but not mountable. The row exists; say why it will not
		// mount rather than pretending the declaration failed.
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
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
		tiles, _ := a.tiles.ListAll(ctx)
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

// orgShareOwner is the org an org share is added to. An org bound to a config
// repo owns its shares in stackr-org.yml, and its next apply would delete one
// added here.
func (a *API) orgShareOwner(ctx context.Context, slug string) (*repo.Org, error) {
	org, err := a.orgs.BySlug(ctx, slug)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "org "+slug+" not found")
	}
	if org.ConfigManaged() {
		return nil, echo.NewHTTPError(http.StatusConflict, org.Slug+" is config-managed; declare the share under storage: in its org file")
	}
	return org, nil
}
