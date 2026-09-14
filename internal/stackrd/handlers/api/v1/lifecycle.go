package v1

// Tile and deployment lifecycle, plus the handful of settings that were panel
// buttons and nothing else. Everything here already existed as a web route; the
// point is that a script can do what a person can, not new behaviour, so each
// handler calls the same service the panel handler calls.

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// stopApp scales the tile's service to zero. Deliberately not a delete: the
// row, its volumes and its replica count all stay, so a later restart brings it
// back as it was.
func (a *API) stopApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.clus.ScaleService(ctx, envnet.ServiceFor(ctx, a.store, t), 0); err != nil {
		return err
	}
	if err := a.store.UpdateTileStatus(ctx, t.ID, "stopped"); err != nil {
		return err
	}
	t.Status = "stopped"
	return c.JSON(http.StatusOK, toAppOut(t))
}

// restartApp bounces the tile in place: a forced service update, not scale 0
// then 1, which would drop the replica count a stopped tile is meant to keep.
func (a *API) restartApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	name := envnet.ServiceFor(ctx, a.store, t)
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "nothing deployed to restart")
	}
	if err := a.clus.RestartService(ctx, name, t.Replicas); err != nil {
		// Bounced but not back up: record what is true, or the tile keeps
		// claiming "running" over a dead one.
		if serr := a.store.UpdateTileStatus(ctx, t.ID, "stopped"); serr != nil {
			slog.Error("tile status not saved", "tile", t.ID, "status", "stopped", "error", serr)
		}
		return err
	}
	if err := a.store.UpdateTileStatus(ctx, t.ID, "running"); err != nil {
		return err
	}
	t.Status = "running"
	return c.JSON(http.StatusOK, toAppOut(t))
}

// toggleCron pauses or resumes a schedule. Idempotent per call in the sense
// that the answer says which state it landed in; a script that wants a
// particular state should read it back rather than count flips.
func (a *API) toggleCron(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if t.Kind != "cron" {
		return echo.NewHTTPError(http.StatusBadRequest, "only cron tiles have a schedule to pause")
	}
	ctx := c.Request().Context()
	status := "paused"
	if t.Status == "paused" {
		status = "idle"
	}
	if err := a.store.UpdateTileStatus(ctx, t.ID, status); err != nil {
		return err
	}
	if a.jobs != nil {
		_ = a.jobs.LoadSchedules(ctx)
	}
	t.Status = status
	return c.JSON(http.StatusOK, toAppOut(t))
}

// rollbackApp redeploys an image tag the tile has run before. The tag is the
// caller's: nothing here guesses a previous one, because "the last good build"
// is a judgement the deployment list is there to support.
func (a *API) rollbackApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
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
	a.engine.Cancel(c.Request().Context(), d.ID)
	return c.JSON(http.StatusOK, toDeploymentOut(d))
}

// appMetrics is the sampled cpu/memory/network series behind the panel's
// graphs. Same windows the panel offers, so the numbers match what a person
// would be looking at.
func (a *API) appMetrics(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	dur := time.Hour
	switch c.QueryParam("range") {
	case "6h":
		dur = 6 * time.Hour
	case "24h":
		dur = 24 * time.Hour
	}
	ms, err := a.store.ListMetrics(c.Request().Context(), "app:"+t.ID, time.Now().Add(-dur))
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
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	if t.Kind != "service" || t.ContainerPort == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "auto domains need a service with a container port")
	}
	env, err := a.store.GetEnvironment(ctx, t.EnvironmentID)
	if err != nil || env == nil {
		return echo.NewHTTPError(http.StatusNotFound, "environment not found")
	}
	if err := (envops.Ops{Store: a.store, RT: a.clus.Runtime(), Cluster: a.clus, PX: a.px}).EnsureAutoDomain(ctx, env, t); err != nil {
		return err
	}
	ds, err := a.store.ListDomainsByTile(ctx, t.ID)
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
	d, err := a.store.GetDomain(ctx, c.Param("id"))
	if err != nil || d == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	t, err := a.requireTile(c, d.TileID, true)
	if err != nil {
		return err
	}
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	var in domainPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.HTTPS != nil {
		if err := a.store.SetDomainHTTPS(ctx, d.ID, *in.HTTPS); err != nil {
			return err
		}
		d.HTTPS = *in.HTTPS
	}
	if in.ForceHTTPS != nil {
		if err := a.store.SetDomainForceHTTPS(ctx, d.ID, *in.ForceHTTPS); err != nil {
			return err
		}
		d.ForceHTTPS = *in.ForceHTTPS
	}
	if in.CertPEM != nil || in.KeyPEM != nil {
		cert, key := strings.TrimSpace(deref(in.CertPEM)), strings.TrimSpace(deref(in.KeyPEM))
		if cert != "" || key != "" {
			if _, err := tls.X509KeyPair([]byte(cert), []byte(key)); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "invalid certificate/key pair: "+err.Error())
			}
		}
		if err := a.store.SetDomainCert(ctx, d.ID, cert, key); err != nil {
			return err
		}
	}
	ds, err := a.store.ListDomainsByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	if err := a.px.WriteApp(t, ds); err != nil {
		return err
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
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	p, err := a.store.GetProvision(ctx, c.Param("pid"))
	if err != nil || p == nil || p.ConsumerTileID != t.ID {
		return echo.NewHTTPError(http.StatusNotFound, "provision not found")
	}
	if err := managedtiles.NewService(a.clus, a.store).Detach(ctx, p); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// setProvisionPublic exposes or hides a slice outside its own network.
func (a *API) setProvisionPublic(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := a.store.GetProvision(ctx, c.Param("id"))
	if err != nil || p == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	inst, err := a.requireTile(c, p.InstanceTileID, true)
	if err != nil {
		return err
	}
	var in publicIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// Every row sharing the bucket name shares its policy, so they move
	// together: one consumer public and its neighbour private is not a state
	// the bucket can actually be in.
	rows := a.provisionRows(ctx, inst.ID, p.DBName)
	svc := managedtiles.NewService(a.clus, a.store)
	for i := range rows {
		if err := svc.SetBucketPublic(ctx, inst, &rows[i], in.Public); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}
	p.Public = in.Public
	return c.JSON(http.StatusOK, a.toSliceOut(c, inst, p))
}

// probeStorage mounts the share through a throwaway sub-path volume on the node
// that will actually mount it at deploy time, and records the outcome. Probing
// the manager for a share hung off a worker proves nothing about the worker.
func (a *API) probeStorage(c echo.Context) error {
	ctx := c.Request().Context()
	st, err := a.store.GetStorage(ctx, c.Param("id"))
	if err != nil || st == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
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
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.requireOrgWrite(ctx, c, s.OrgID); err != nil {
		return err
	}
	if err := a.rejectManaged(ctx, s.ID); err != nil {
		return err
	}
	var in stackPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		slug := repo.Slugify(name)
		if slug == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "name needs at least one letter or number")
		}
		if other, _ := a.store.GetStackBySlug(ctx, s.OrgID, slug); other != nil && other.ID != s.ID {
			return echo.NewHTTPError(http.StatusConflict, "a stack with that name already exists in this organization")
		}
		if slug != s.Slug {
			// Writes the row itself, stops and redeploys. Inline is fine: the
			// redeploy only enqueues.
			if err := a.applier.RenameStack(ctx, s, name, &stackconf.Plan{}); err != nil {
				return err
			}
		} else {
			s.Name = name
		}
	}
	if in.Description != nil {
		s.Description = *in.Description
	}
	if err := a.store.UpdateStack(ctx, s); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, stackOut{ID: s.ID, Name: s.Name, Description: s.Description})
}

// getPREnv and putPREnv are the per-stack pull-request environment settings.
// The webhook secret is write-only here: it is a credential, and the panel is
// where it is read once after rotation.
func (a *API) getPREnv(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	cfg := envops.LoadPRConfig(c.Request().Context(), a.store, s.ID)
	return c.JSON(http.StatusOK, prEnvOut{Enabled: cfg.Enabled,
		Comment: !cfg.NoComment, Status: !cfg.NoStatus, SecretSet: cfg.Secret != ""})
}

func (a *API) putPREnv(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.requireOrgWrite(ctx, c, s.OrgID); err != nil {
		return err
	}
	var in prEnvIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	cfg := envops.LoadPRConfig(ctx, a.store, s.ID)
	cfg.Enabled = in.Enabled
	if in.Comment != nil {
		cfg.NoComment = !*in.Comment
	}
	if in.Status != nil {
		cfg.NoStatus = !*in.Status
	}
	if in.RotateSecret || cfg.Secret == "" {
		cfg.Secret = secrets.RandomHex(24)
	}
	if err := envops.SavePRConfig(ctx, a.store, s.ID, cfg); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, prEnvOut{Enabled: cfg.Enabled,
		Comment: !cfg.NoComment, Status: !cfg.NoStatus, SecretSet: cfg.Secret != ""})
}
