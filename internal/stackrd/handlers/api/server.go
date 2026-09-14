// Package api mounts the REST surface. It owns route registration and the
// middleware chain (JSON errors, then x-api-key auth); the operations
// themselves live in api/v1, which declares each route and its OpenAPI entry
// from the same call so the published spec cannot drift from what is enforced.
package api

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/api/handler/health"
	v1 "github.com/FyrmForge/stackr/internal/stackrd/handlers/api/v1"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/hostmetrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/nodes"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Deps holds the dependencies for API route registration.
type Deps struct {
	Store    repo.Store
	Engine   *deploy.Engine
	Runtime  *runtime.Runtime
	Cluster  *cluster.Cluster // one door for every docker call (docs/plans/35-cluster.md)
	Proxy    *proxy.Proxy
	Jobs     *jobs.Service
	Backups  *backup.Service
	Forwards *forward.Registry
	Notifier *notify.Notifier
	// Applier drives config-as-code. Shared with the web router so the plan
	// the canvas shows and the plan the API approves are the same object.
	Applier stackconf.Applier
	// Work is the durable job runner; applies are enqueued, not run inline.
	Work *workqueue.Queue
	// RegistrySigner mints managed-registry tokens; nil when it would not load.
	RegistrySigner *registry.Signer
	// Nodes takes the host metrics samples the node agents push.
	Nodes *nodes.Service
	// DataDir is where the panel keeps its copy of the agent runtime key,
	// which is what the sample push is authenticated against.
	DataDir string
}

// RegisterRoutes registers all API route handlers on the server.
func RegisterRoutes(srv *server.Server, deps *Deps) {
	api := srv.Echo().Group("/api")
	api.Use(middleware.Logging())

	healthHandler := health.NewHandler(deps.Store)
	api.GET("/health", healthHandler.Health)

	// The agents' sample endpoint sits outside the v1 group: an agent holds
	// the shared runtime key, not an API key, and it is a push from inside
	// the overlay rather than part of the public API
	// (docs/plans/31-node-agent-open-questions.md, transport).
	if deps.Nodes != nil {
		api.POST("/v1/nodes/samples", nodeSamples(deps.Nodes, deps.DataDir))
	}

	apiV1 := v1.New(deps.Store, deps.Engine, deps.Runtime, deps.Cluster, deps.Proxy, deps.Jobs, deps.Backups, deps.Forwards, deps.Notifier, deps.Applier).
		WithWork(deps.Work).
		WithRegistrySigner(deps.RegistrySigner)
	api.GET("/openapi.json", apiV1.SpecHandler)
	g := api.Group("/v1", apiV1.JSONErrors, apiV1.KeyAuth)
	apiV1.Register(g)
}

// nodeSamples stores one host metrics sample pushed by a node agent.
func nodeSamples(svc *nodes.Service, dataDir string) echo.HandlerFunc {
	return func(c echo.Context) error {
		key := agent.ReadKey(dataDir)
		if key == "" || strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ") != key {
			return echo.NewHTTPError(http.StatusUnauthorized, "bad agent key")
		}
		var req agent.SampleReq
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		if req.NodeID == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "no node id")
		}
		if err := svc.StoreSample(c.Request().Context(), req.NodeID, req.TS, hostmetrics.HostSample{
			CPUPct: req.CPUPct, MemBytes: req.MemBytes,
			RxBps: req.RxBps, TxBps: req.TxBps, DiskUsed: req.DiskUsed,
		}); err != nil {
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		}
		return c.NoContent(http.StatusOK)
	}
}
