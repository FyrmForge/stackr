package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

// Connectors of a registered kind, and the DNS capability's verbs on them,
// at two levels: server ones (admin, /admin/...) and an org's (owner,
// /orgs/:org/...). The same handlers serve both; level says which.

type (
	// ManageIn is who may use a server connector's domain: these orgs,
	// or every org when empty. An org connector takes no org_ids. Adopt
	// is the existing records to take over, as the plan lists them.
	ManageIn struct {
		OrgIDs []string         `json:"org_ids"`
		Adopt  []service.DNSRef `json:"adopt"`
	}
	// TakeOverIn is the domain typed out.
	TakeOverIn struct {
		Confirm string `json:"confirm"`
	}
)

func (ManageIn) BodyOptional() {}

// Level picks whose connectors a handler reaches.
type Level bool

const (
	ServerLevel Level = true
	OrgLevel    Level = false
)

func (l Level) org(c echo.Context) *string {
	if l == ServerLevel {
		return nil
	}
	id := orgID(c)
	return &id
}

func (h *H) DNSConnectors(l Level) Endpoint {
	return Get(func(c echo.Context) ([]service.Connector, error) {
		return list(h.Orch.DNSConnectors(rc(c), l.org(c)))
	})
}

func (h *H) DNSDomains(l Level) Endpoint {
	return Get(func(c echo.Context) ([]service.DNSDomain, error) {
		return list(h.Orch.DNSDomains(rc(c), l.org(c), c.Param("connector")))
	})
}

func (h *H) ManageDNSDomain(l Level) Endpoint {
	return JSON(200, func(c echo.Context, in ManageIn) (service.DNSDomain, error) {
		return h.Orch.ManageDNSDomainAdopting(rc(c), l.org(c), c.Param("connector"), c.Param("zone"), "", in.OrgIDs, in.Adopt)
	})
}

// PlanDNSDomain is what managing the zone would write and the existing
// records it may adopt (?org_id=, repeated, as org_ids in the body).
func (h *H) PlanDNSDomain(l Level) Endpoint {
	return Get(func(c echo.Context) (service.DNSManagePlan, error) {
		return h.Orch.PlanManageDNSDomain(rc(c), l.org(c), c.Param("connector"), c.Param("zone"), "", c.QueryParams()["org_id"])
	})
}

func (h *H) UnmanageDNSDomain(l Level) Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.UnmanageDNSDomain(rc(c), l.org(c), c.Param("connector"), c.Param("zone"))
	})
}

func (h *H) TakeOverDNSDomain(l Level) Endpoint {
	return Done(func(c echo.Context, in TakeOverIn) error {
		return h.Orch.TakeOverDNSDomain(rc(c), l.org(c), c.Param("connector"), c.Param("zone"), in.Confirm)
	})
}

// RemoveConnector removes an org's connector of a registered kind, each
// capability cleaning up first.
func (h *H) RemoveConnector(l Level) Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RemoveConnector(rc(c), l.org(c), c.Param("connector"))
	})
}
