package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// RotateKey gives the caller's key a new secret (shown once); the old one
// stops working, the id, scope, level and expiry stay.
func (h *H) RotateKey() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (KeyOut, error) {
		tok, k, err := h.Orch.RotateKey(rc(c), middleware.Principal(c), c.Param("key"))
		return KeyOut{tok, k}, err
	})
}

// OrgKeys lists every key bound to the org, whoever minted it.
func (h *H) OrgKeys() Endpoint {
	return Get(func(c echo.Context) ([]service.APIKey, error) {
		return paged[service.APIKey](c)(h.Orch.OrgKeys(rc(c), orgID(c)))
	}).Paged()
}

// RevokeOrgKey revokes a key bound to the org, whoever owns it.
func (h *H) RevokeOrgKey() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RevokeOrgKey(rc(c), middleware.Principal(c), orgID(c), c.Param("key"))
	})
}

// AllKeys lists every key on the server (admin).
func (h *H) AllKeys() Endpoint {
	return Get(func(c echo.Context) ([]service.APIKey, error) {
		return paged[service.APIKey](c)(h.Orch.AllKeys(rc(c)))
	}).Paged()
}

// RevokeAnyKey revokes any key on the server (admin).
func (h *H) RevokeAnyKey() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RevokeOrgKey(rc(c), middleware.Principal(c), "", c.Param("key"))
	})
}
