package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	// DomainResourceIn is a new resource; its level and owner are the path's
	// (an org, a stack, or /admin for the instance).
	DomainResourceIn struct {
		Host                string `json:"host"`
		IncludeEnvOnDefault bool   `json:"include_env_on_default"`
		ACMEEmail           string `json:"acme_email"` // "" = the instance's ACME account
	}
	// DomainResourceSetIn is the whole editable state: both fields are
	// written. The host and level never move.
	// ponytail: a partial body clears the field it leaves out; pointer
	// fields when a client sends partials.
	DomainResourceSetIn struct {
		IncludeEnvOnDefault bool   `json:"include_env_on_default"`
		ACMEEmail           string `json:"acme_email"`
	}
)

// DomainResources is the org's rows, then the instance's.
func (h *H) DomainResources() Endpoint {
	return Get(func(c echo.Context) ([]service.DomainResource, error) {
		return list(h.Orch.DomainResources(rc(c), orgID(c)))
	})
}

func (h *H) CreateOrgDomainResource() Endpoint {
	return JSON(201, func(c echo.Context, in DomainResourceIn) (service.DomainResource, error) {
		return h.Orch.CreateDomainResource(rc(c), "org", orgID(c), in.Host, in.IncludeEnvOnDefault, in.ACMEEmail)
	})
}

func (h *H) CreateStackDomainResource() Endpoint {
	return JSON(201, func(c echo.Context, in DomainResourceIn) (service.DomainResource, error) {
		return h.Orch.CreateDomainResource(rc(c), "stack", stackID(c), in.Host, in.IncludeEnvOnDefault, in.ACMEEmail)
	})
}

// UpdateDomainResource serves the org route and the admin one: the org
// route's middleware already refused another org's id.
func (h *H) UpdateDomainResource() Endpoint {
	return JSON(200, func(c echo.Context, in DomainResourceSetIn) (service.DomainResource, error) {
		return h.Orch.UpdateDomainResource(rc(c), c.Param("resource"), in.IncludeEnvOnDefault, in.ACMEEmail)
	})
}

func (h *H) DeleteDomainResource() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeleteDomainResource(rc(c), c.Param("resource"))
	})
}

// AllDomainResources is every resource on the server (admin).
func (h *H) AllDomainResources() Endpoint {
	return Get(func(c echo.Context) ([]service.DomainResource, error) {
		return list(h.Orch.AllDomainResources(rc(c)))
	})
}

func (h *H) CreateInstanceDomainResource() Endpoint {
	return JSON(201, func(c echo.Context, in DomainResourceIn) (service.DomainResource, error) {
		return h.Orch.CreateDomainResource(rc(c), "instance", "", in.Host, in.IncludeEnvOnDefault, in.ACMEEmail)
	})
}
