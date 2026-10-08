package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

// HostGrantOut is the elevated access a server admin approved for a stack, and
// what its waiting jobs still ask for.
type HostGrantOut = service.HostGrant

// HostGrant is the stack's grant; a stack with none reads as not granted.
func (h *H) HostGrant() Endpoint {
	return Get(func(c echo.Context) (HostGrantOut, error) { return h.Orch.HostGrant(rc(c), stackID(c)) })
}

// ApproveHostGrantIn is the pending set the admin was shown, and the part of
// it to grant.
type ApproveHostGrantIn struct {
	Pending []string `json:"pending"` // the pending list of the GET; the approval is a 409 when the ask has changed
	Grant   []string `json:"grant"`   // a subset of pending to grant, the rest keeps waiting; empty = all of pending
}

// ApproveHostGrant (server admin) records the set the stack's parked jobs
// asked for and requeues them. The body carries the pending set shown; a
// different current ask is a 409 and grants nothing.
func (h *H) ApproveHostGrant() Endpoint {
	return JSON(200, func(c echo.Context, in ApproveHostGrantIn) (HostGrantOut, error) {
		return h.Orch.ApproveHostGrant(rc(c), stackID(c), who(c), in.Pending, in.Grant)
	})
}

// RevokeHostGrant (server admin) deletes the grant, or one tile's lines with
// ?tile=<slug>: running tiles stay, the next deploy that needs the access
// parks again.
func (h *H) RevokeHostGrant() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RevokeHostGrant(rc(c), stackID(c), c.QueryParam("tile"))
	})
}

// HostGrants (server admin) lists every stack's grant and pending ask.
func (h *H) HostGrants() Endpoint {
	return Get(func(c echo.Context) ([]HostGrantOut, error) { return list(h.Orch.ListHostGrants(rc(c))) })
}
