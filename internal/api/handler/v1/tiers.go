package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	SlugIn struct {
		Slug string `json:"slug"`
	}
	SlugsIn struct {
		Slugs []string `json:"slugs"`
	} // bottom rung first
	LockIn struct {
		Locked bool `json:"locked"`
	}
)

// ---- tiers ----

func (h *H) Tiers() Endpoint {
	return Get(func(c echo.Context) ([]service.Tier, error) { return list(h.Orch.Tiers(rc(c), orgID(c))) })
}

func (h *H) CreateTier() Endpoint {
	return JSON(201, func(c echo.Context, in SlugIn) (service.Tier, error) {
		return h.Orch.CreateTier(rc(c), orgID(c), in.Slug)
	})
}

func (h *H) RenameTier() Endpoint {
	return JSON(200, func(c echo.Context, in SlugIn) (service.Tier, error) {
		return h.Orch.RenameTier(rc(c), orgID(c), c.Param("tier"), in.Slug)
	})
}

func (h *H) ReorderTiers() Endpoint {
	return Done(func(c echo.Context, in SlugsIn) error { return h.Orch.ReorderTiers(rc(c), orgID(c), in.Slugs) })
}

func (h *H) DeleteTier() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteTier(rc(c), orgID(c), c.Param("tier")) })
}

func (h *H) SetTierLock() Endpoint {
	return JSON(200, func(c echo.Context, in LockIn) (service.Tier, error) {
		return h.Orch.SetTierLock(rc(c), orgID(c), c.Param("tier"), in.Locked)
	})
}

func (h *H) SetEnvLock() Endpoint {
	return JSON(200, func(c echo.Context, in LockIn) (service.Environment, error) {
		return h.Orch.SetEnvLock(rc(c), envID(c), in.Locked)
	})
}
