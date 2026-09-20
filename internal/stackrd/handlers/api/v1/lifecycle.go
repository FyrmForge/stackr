package v1

// Tile and deployment lifecycle, plus the handful of settings that were panel
// buttons and nothing else. Everything here already existed as a web route; the
// point is that a script can do what a person can, not new behaviour, so each
// handler calls the same service the panel handler calls.

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// stopApp takes the tile out of service. Deliberately not a delete: the row,
// its volumes and its replica count all stay, so a later restart brings it
// back as it was. A cron parks as "paused" — see TileLifecycleService.Stop
// for why "stopped" was the wrong state for a schedule.
func (a *API) stopApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.life.Stop(c.Request().Context(), t); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, toAppOut(t))
}

// restartApp bounces the tile in place: a forced service update, not scale 0
// then 1, which would drop the replica count a stopped tile is meant to keep.
func (a *API) restartApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.life.Restart(c.Request().Context(), t); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, toAppOut(t))
}

// toggleCron pauses or resumes a schedule. The answer says which state it
// landed in; a script that wants a particular state should read it back
// rather than count flips.
func (a *API) toggleCron(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if _, err := a.life.ToggleCron(c.Request().Context(), t); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, toAppOut(t))
}

// rollbackApp redeploys an image tag the tile has run before. The tag is the
// caller's: nothing here guesses a previous one, because "the last good build"
// is a judgement the deployment list is there to support.
func (a *API) rollbackApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in rollbackIn
	if err := c.Bind(&in); err != nil || strings.TrimSpace(in.ImageTag) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "image_tag required")
	}
	id, err := a.engine.EnqueueRollback(c.Request().Context(), t, strings.TrimSpace(in.ImageTag))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, deployAccepted{Deployment: id})
}

// cancelDeployment stops a build or roll-out in flight.
func (a *API) cancelDeployment(c echo.Context) error {
	d, err := a.loadDeployment(c, c.Param("id"))
	if err != nil {
		return err
	}
	a.deploys.Cancel(c.Request().Context(), d.ID)
	return c.JSON(http.StatusOK, toDeploymentOut(d))
}

// appMetrics is the sampled cpu/memory/network series behind the panel's
// graphs. Same windows the panel offers, so the numbers match what a person
// would be looking at.
func (a *API) appMetrics(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	// One range parser and one samples key, shared with the panel's picker:
	// these were two switches over the same three strings, so a window added
	// to one of them left the other quietly answering 1h.
	_, dur := service.MetricRange(c.QueryParam("range"))
	ms, err := a.telemetry.Samples(c.Request().Context(), service.TileRef(t.ID), dur)
	if err != nil {
		return err
	}
	out := make([]metricOut, 0, len(ms))
	for i := range ms {
		out = append(out, metricOut{At: ms[i].TS, CPUPercent: ms[i].CPUPct,
			MemBytes: ms[i].MemBytes, RxBps: ms[i].RxBps, TxBps: ms[i].TxBps})
	}
	return c.JSON(http.StatusOK, out)
}

// createAutoDomain generates this tile's hostname under whichever domain
// resource its environment nests below. A no-op with no resource to nest under
// is reported rather than answered 201: the caller would otherwise read an
// empty success as a domain it does not have.
func (a *API) createAutoDomain(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	env, err := a.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if err := a.domains.AddAuto(ctx, env, t, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	ds, err := a.domains.ForTile(ctx, t.ID)
	if err != nil {
		return err
	}
	for i := range ds {
		if ds[i].Auto {
			return c.JSON(http.StatusCreated, toDomainOut(&ds[i]))
		}
	}
	return echo.NewHTTPError(http.StatusBadRequest,
		"no domain resource to nest under; add one at instance, organization or stack level first")
}

// patchDomain flips HTTPS and installs or clears a custom certificate, the two
// per-domain knobs the panel has. A cert and key that do not pair are refused
// here rather than at the proxy, where the failure is a silent 5xx on one host.
func (a *API) patchDomain(c echo.Context) error {
	ctx := c.Request().Context()
	d, err := a.domains.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	t, err := a.tile(c, d.TileID)
	if err != nil {
		return err
	}
	var in domainPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	d, staged, err := a.domains.SetTLS(ctx, t, d.ID, service.TLSPatch{
		HTTPS: in.HTTPS, ForceHTTPS: in.ForceHTTPS, CertPEM: in.CertPEM, KeyPEM: in.KeyPEM,
	}, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		return echo.NewHTTPError(http.StatusConflict,
			"this stack stages edits for review; change the domain from the canvas")
	}
	return c.JSON(http.StatusOK, toDomainOut(d))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// detachProvision unhooks one consumer from a slice and keeps the data. The
// slice itself, and every other consumer of it, is untouched; dropping it is
// DELETE /provisions/{id}.
func (a *API) detachProvision(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	if err := a.slices.Detach(ctx, t, c.Param("pid")); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// setProvisionPublic exposes or hides a slice outside its own network.
func (a *API) setProvisionPublic(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := a.slices.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	inst, err := a.tile(c, p.InstanceTileID)
	if err != nil {
		return err
	}
	var in publicIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if err := a.slices.SetPublic(ctx, inst, p, in.Public); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, a.toSliceOut(c, inst, p))
}

// probeStorage mounts the share through a throwaway sub-path volume on the node
// that will actually mount it at deploy time, and records the outcome. Probing
// the manager for a share hung off a worker proves nothing about the worker.
func (a *API) probeStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := a.storage.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	node, err := a.clus.NodeOfStorage(ctx, st)
	if err == nil {
		p := repo.StoragePath{ID: uuid.New().String(), StorageID: st.ID, Subpath: ""}
		err = storagetiles.Probe(ctx, a.clus, node, st, &p)
		_ = a.clus.RemoveVolume(ctx, node, repo.StorageVolume(p.ID))
	}
	if err != nil {
		st.Status, st.StatusMsg = "error", err.Error()
	} else {
		st.Status, st.StatusMsg = "ok", ""
	}
	if err := a.store.UpdateStorage(ctx, st); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, a.storageOut(c, st))
}

// patchStack renames a stack. The slug moves with the name, so every URL, CLI
// path and container name under it moves too; a config-managed stack is refused
// because its file owns the name.
//
// The rename goes through the config engine's RenameStack, which is the only
// implementation that finishes the job: swarm cannot rename a service, so the
// running tiles have to be stopped under the old name and redeployed under
// the new one. Writing the slug alone left them serving under a name nothing
// resolved any more.
func (a *API) patchStack(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var in stackPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// The managed refusal, the slug rules and the whole-way rename are the
	// service's; rejectManaged here would ask the same question twice.
	if err := a.stacks.Update(ctx, s, service.StackPatch{
		Name: in.Name, Description: in.Description,
	}, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, stackOut{ID: s.ID, Name: s.Name, Description: s.Description})
}

// getPREnv and putPREnv are the per-stack pull-request environment settings.
// The webhook secret is write-only here: it is a credential, and the panel is
// where it is read once after rotation.
func (a *API) getPREnv(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	cfg := repo.LoadPRConfig(c.Request().Context(), a.store, s.ID)
	return c.JSON(http.StatusOK, prEnvOut{Enabled: cfg.Enabled,
		Comment: !cfg.NoComment, Status: !cfg.NoStatus, SecretSet: cfg.Secret != ""})
}

func (a *API) putPREnv(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var in prEnvIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// Sparse: `enabled` is a pointer now, so omitting it leaves it alone.
	// This replaced it whatever the caller sent, which is why the CLI
	// read-merge-writes around the route.
	cfg, err := a.prenvs.Update(ctx, s, service.PREnvPatch{
		Enabled: in.Enabled, Comment: in.Comment, Status: in.Status,
		RotateSecret: in.RotateSecret,
	}, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, prEnvOut{Enabled: cfg.Enabled,
		Comment: !cfg.NoComment, Status: !cfg.NoStatus, SecretSet: cfg.Secret != ""})
}
