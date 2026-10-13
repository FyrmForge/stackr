package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// The cluster trust verbs behind `stackr cluster trust`, `rotate-root`,
// `break-glass` and `revoke-controller-cert`. Server admins only; the
// per-node rejoin command is the existing node rejoin route.

// ClusterTrust is the root's dates, any rollover under way and the nodes
// break glass left unjoined.
func (h *H) ClusterTrust() Endpoint {
	return Get(func(c echo.Context) (service.ClusterTrust, error) {
		return h.Orch.ClusterTrust(rc(c), middleware.Principal(c))
	})
}

// RotateRoot starts a planned root rollover; the old root stays trusted
// until every node holds a new-root cert.
func (h *H) RotateRoot() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RotateRoot(rc(c), middleware.Principal(c))
	})
}

// FinishRollover ends a stuck rollover now; nodes still on the old root need
// a rejoin.
func (h *H) FinishRollover() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.FinishRollover(rc(c), middleware.Principal(c))
	})
}

// BreakGlass wipes the root and every WireGuard key. Tunnels between nodes
// stay down until each node runs its rejoin command.
func (h *H) BreakGlass() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.BreakGlass(rc(c), middleware.Principal(c))
	})
}

// RevokeControllerCert gives the controller a new cert and stops the old one
// working on every worker.
func (h *H) RevokeControllerCert() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RevokeControllerCert(rc(c), middleware.Principal(c))
	})
}
