package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

// DNS connectors, at two levels: server ones (admin, /admin/dns-connectors)
// and an org's (owner, /orgs/:org/dns-connectors). The same handlers serve
// both; level says which.

type (
	// NamecheapIn is a Namecheap account. client_ip empty is stackr's own
	// public IPv4.
	NamecheapIn struct {
		User     string `json:"user"`
		Key      string `json:"key"`
		ClientIP string `json:"client_ip"`
	}
	// ManageIn is who may use a server connector's domain: these orgs,
	// or every org when empty. An org connector takes no body.
	ManageIn struct {
		OrgIDs []string `json:"org_ids"`
	}
	// TakeOverIn is the domain typed out.
	TakeOverIn struct {
		Confirm string `json:"confirm"`
	}
)

func (ManageIn) BodyOptional() {}

// Level picks whose DNS connectors a handler reaches.
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

func (h *H) AddNamecheap(l Level) Endpoint {
	return JSON(201, func(c echo.Context, in NamecheapIn) (service.DNSConnector, error) {
		return h.Orch.AddDNSConnector(rc(c), l.org(c), service.DNSNamecheap, service.DNSAccount(in))
	}).Limit(64 << 10)
}

func (h *H) DNSDomains(l Level) Endpoint {
	return Get(func(c echo.Context) ([]service.DNSDomain, error) {
		return list(h.Orch.DNSDomains(rc(c), l.org(c), c.Param("connector")))
	})
}

func (h *H) ManageDNSDomain(l Level) Endpoint {
	return JSON(200, func(c echo.Context, in ManageIn) (service.DNSDomain, error) {
		return h.Orch.ManageDNSDomain(rc(c), l.org(c), c.Param("connector"), c.Param("zone"), in.OrgIDs)
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

func (h *H) DeleteDNSConnector(l Level) Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeleteDNSConnector(rc(c), l.org(c), c.Param("connector"))
	})
}
