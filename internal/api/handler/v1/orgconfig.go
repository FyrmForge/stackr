package v1

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	OrgConfigRepoIn struct {
		ConnectorID string `json:"connector_id"`
		Repo        string `json:"repo"` // "" unbinds
		Branch      string `json:"branch"`
		Path        string `json:"path"`
		Auto        bool   `json:"auto"`
	}
	OrgFileIn struct {
		File string `json:"file"` // a stackr-org.yml
	}
)

// maxOrgFile caps a plan-preview body, as v0 did.
const maxOrgFile = 2 << 20

func (h *H) SetOrgConfigRepo() Endpoint {
	return JSON(200, func(c echo.Context, in OrgConfigRepoIn) (service.Org, error) {
		return h.Orch.SetOrgConfigRepo(rc(c), orgID(c), in.ConnectorID, in.Repo, in.Branch, in.Path, in.Auto)
	})
}

func (h *H) PlanOrgConfig() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (service.OrgPlan, error) {
		return h.Orch.PlanOrgConfig(rc(c), orgID(c))
	})
}

func (h *H) PreviewOrgConfig() Endpoint {
	return JSON(200, func(c echo.Context, in OrgFileIn) (service.OrgConfigPlan, error) {
		return h.Orch.PreviewOrgConfig(rc(c), orgID(c), []byte(in.File))
	}).Limit(maxOrgFile)
}

func (h *H) OrgPlans() Endpoint {
	return Get(func(c echo.Context) ([]service.OrgPlan, error) {
		return list(h.Orch.OrgPlans(rc(c), orgID(c), 20))
	})
}

func (h *H) OrgPlan() Endpoint {
	return Get(func(c echo.Context) (service.OrgPlan, error) {
		return h.Orch.OrgPlan(rc(c), c.Param("plan"))
	})
}

func (h *H) ApproveOrgPlan() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.ApproveOrgPlan(rc(c), c.Param("plan"))
	})
}

func (h *H) RejectOrgPlan() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (service.OrgPlan, error) {
		return h.Orch.RejectOrgPlan(rc(c), c.Param("plan"))
	})
}

// ExportOrgConfig answers the org as a stackr-org.yml download.
func (h *H) ExportOrgConfig() Endpoint {
	return Streamed("application/yaml", func(c echo.Context) error {
		b, err := h.Orch.ExportOrgConfig(rc(c), orgID(c))
		if err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="stackr-org.yml"`)
		return c.Blob(http.StatusOK, "application/yaml", b)
	})
}
