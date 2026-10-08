package v1

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	ServerConfigRepoIn struct {
		ConnectorID string `json:"connector_id"` // a server connector's id
		Repo        string `json:"repo"`         // "" unbinds
		Branch      string `json:"branch"`
		Path        string `json:"path"`
		Auto        bool   `json:"auto"`
	}
	ServerFileIn struct {
		File string `json:"file"` // a stackr-server.yml
	}
	DomainRenameIn struct {
		Host string `json:"host"` // the resource's new host
	}
)

func (h *H) ServerConfigRepo() Endpoint {
	return Get(func(c echo.Context) (service.ServerBinding, error) {
		return h.Orch.ServerConfigBinding(rc(c))
	})
}

func (h *H) SetServerConfigRepo() Endpoint {
	return JSON(200, func(c echo.Context, in ServerConfigRepoIn) (service.ServerBinding, error) {
		return h.Orch.BindServerConfig(rc(c), in.ConnectorID, in.Repo, in.Branch, in.Path, in.Auto)
	})
}

func (h *H) PlanServerConfig() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (service.ServerPlan, error) {
		return h.Orch.PlanServerConfig(rc(c))
	})
}

// PlanServerFile stores a plan of a local file, tagged as not from the repo.
func (h *H) PlanServerFile() Endpoint {
	return JSON(200, func(c echo.Context, in ServerFileIn) (service.ServerPlan, error) {
		return h.Orch.PlanServerFile(rc(c), []byte(in.File))
	}).Limit(maxOrgFile)
}

func (h *H) PreviewServerConfig() Endpoint {
	return JSON(200, func(c echo.Context, in ServerFileIn) (service.ServerConfigPlan, error) {
		return h.Orch.PreviewServerConfig(rc(c), []byte(in.File))
	}).Limit(maxOrgFile)
}

func (h *H) ServerPlans() Endpoint {
	return Get(func(c echo.Context) ([]service.ServerPlan, error) {
		return list(h.Orch.ServerPlans(rc(c), 20))
	})
}

func (h *H) ServerPlan() Endpoint {
	return Get(func(c echo.Context) (service.ServerPlan, error) {
		return h.Orch.ServerPlan(rc(c), c.Param("plan"))
	})
}

func (h *H) ApproveServerPlan() Endpoint {
	return Job(func(c echo.Context, in ApproveIn) (service.Job, error) {
		opts := in.opts()
		opts.ApproverID = who(c) // owns the orgs the file creates
		return h.Orch.ApproveServerPlan(rc(c), c.Param("plan"), opts)
	})
}

func (h *H) RejectServerPlan() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (service.ServerPlan, error) {
		return h.Orch.RejectServerPlan(rc(c), c.Param("plan"))
	})
}

// ExportServerConfig answers the server as a stackr-server.yml download.
func (h *H) ExportServerConfig() Endpoint {
	return Streamed("application/yaml", func(c echo.Context) error {
		b, err := h.Orch.ExportServerConfig(rc(c))
		if err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="stackr-server.yml"`)
		return c.Blob(http.StatusOK, "application/yaml", b)
	})
}

// RenameDomainResource serves the org route and the admin one, like
// UpdateDomainResource: the org route's middleware already refused another
// org's id.
func (h *H) RenameDomainResource() Endpoint {
	return JSON(200, func(c echo.Context, in DomainRenameIn) (service.DomainResource, error) {
		return h.Orch.RenameDomainResource(rc(c), c.Param("resource"), in.Host)
	})
}
