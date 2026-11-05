package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

type (
	SlugIn struct {
		Slug string `json:"slug"`
	}
	SlugsIn struct {
		Slugs []string `json:"slugs"`
	} // bottom rung first
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

// TierPatch is the fields of a tier that PATCH changes; a field left out
// stays. The rename applies first, then the lock.
type TierPatch struct {
	Slug   *string `json:"slug,omitempty"`
	Locked *bool   `json:"locked,omitempty"`
}

func (h *H) UpdateTier() Endpoint {
	return JSON(200, func(c echo.Context, in TierPatch) (service.Tier, error) {
		slug := c.Param("tier")
		if in.Slug == nil && in.Locked == nil {
			return service.Tier{}, errs.Invalidf("", "send slug, locked or both")
		}
		var t service.Tier
		if in.Slug != nil {
			var err error
			if t, err = h.Orch.RenameTier(rc(c), orgID(c), slug, *in.Slug); err != nil {
				return t, err
			}
			slug = t.Slug
		}
		if in.Locked != nil {
			return h.Orch.SetTierLock(rc(c), orgID(c), slug, *in.Locked)
		}
		return t, nil
	})
}

func (h *H) ReorderTiers() Endpoint {
	return Done(func(c echo.Context, in SlugsIn) error { return h.Orch.ReorderTiers(rc(c), orgID(c), in.Slugs) })
}

func (h *H) DeleteTier() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteTier(rc(c), orgID(c), c.Param("tier")) })
}
