package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// PGRecipeBody is a postgres extension recipe: the body of PUT
// /admin/pg-extensions/:name and the rows of the list. For a built-in only
// enabled and share are read.
type PGRecipeBody struct {
	Name       string            `json:"name"`
	Builtin    bool              `json:"builtin"`
	Enabled    bool              `json:"enabled"`
	Majors     []int             `json:"majors"`
	Packages   []string          `json:"packages"`
	PackageURL string            `json:"package_url"`
	SHA256     map[string]string `json:"sha256"`
	Version    string            `json:"version"`
	Extensions []string          `json:"extensions"`
	Preload    []string          `json:"preload"`
	Requires   []string          `json:"requires"`
	Share      []string          `json:"share"` // ["all"] or org ids
}

func pgRecipeBody(v service.PGRecipeView) PGRecipeBody {
	return PGRecipeBody{Name: v.Name, Builtin: v.Builtin, Enabled: v.Enabled, Majors: v.Majors,
		Packages: v.Packages, PackageURL: v.PackageURL, SHA256: v.SHA256, Version: v.Version,
		Extensions: v.Extensions, Preload: v.Preload, Requires: v.Requires, Share: v.Share}
}

// PGRecipes lists the built-in and the admin-added recipes.
func (h *H) PGRecipes() Endpoint {
	return Get(func(c echo.Context) ([]PGRecipeBody, error) {
		vs, err := h.Orch.PGRecipes(rc(c))
		out := make([]PGRecipeBody, 0, len(vs))
		for _, v := range vs {
			out = append(out, pgRecipeBody(v))
		}
		return out, err
	})
}

// SetPGRecipe adds or rewrites a recipe, or switches a built-in on or off.
func (h *H) SetPGRecipe() Endpoint {
	return JSON(200, func(c echo.Context, in PGRecipeBody) (PGRecipeBody, error) {
		if in.Name != "" && in.Name != c.Param("name") {
			return PGRecipeBody{}, errs.Invalidf("name", "name in the body must match the one in the path")
		}
		v, err := h.Orch.SetPGRecipe(rc(c), middleware.Principal(c), service.PGRecipeView{
			Name: c.Param("name"), Enabled: in.Enabled, Majors: in.Majors, Packages: in.Packages,
			PackageURL: in.PackageURL, SHA256: in.SHA256, Version: in.Version, Extensions: in.Extensions,
			Preload: in.Preload, Requires: in.Requires, Share: in.Share})
		return pgRecipeBody(v), err
	})
}

// DeletePGRecipe removes an admin recipe, or a built-in's switch-off row.
func (h *H) DeletePGRecipe() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeletePGRecipe(rc(c), middleware.Principal(c), c.Param("name"))
	})
}

// PGPkgChangeBody is one package's version change; from "" is a new package,
// to "" a removed one.
type PGPkgChangeBody struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// PGExtPlanBody is an instance's recipe plan: the id apply must name.
type PGExtPlanBody struct {
	ID       string            `json:"id"`
	Hash     string            `json:"hash"`
	Lines    []string          `json:"lines"`
	Build    bool              `json:"build"`
	Restart  bool              `json:"restart"`
	Versions []PGPkgChangeBody `json:"versions"`
	PGUpdate string            `json:"pg_update"`
	Slices   []string          `json:"slices"`
	Blockers []string          `json:"blockers"`
}

// PGExtApplyIn names the reviewed plan to apply; confirm is needed when the
// plan restarts the instance.
type PGExtApplyIn struct {
	Plan    string `json:"plan"`
	Confirm bool   `json:"confirm"`
}

// PlanPGExtensions queues the plan job of a recipe change; the plan is read
// from GET .../extensions/plan once the job is done.
func (h *H) PlanPGExtensions() Endpoint {
	return JSON(200, func(c echo.Context, in PGExtIn) (service.Job, error) {
		return h.Orch.PlanPGExtensions(rc(c), middleware.Principal(c), tileID(c),
			service.PGExtIn{Image: in.Image, Preload: in.Preload, Extensions: in.Extensions})
	})
}

// PGExtPlanOf reads the instance's last plan; 404 when it has none.
func (h *H) PGExtPlanOf() Endpoint {
	return Get(func(c echo.Context) (PGExtPlanBody, error) {
		ms, err := h.Orch.OperatedInstances(rc(c), envID(c))
		if err != nil {
			return PGExtPlanBody{}, err
		}
		for _, m := range ms {
			if m.TileID != tileID(c) {
				continue
			}
			p, err := h.Orch.LastPGExtPlan(rc(c), m.ID)
			if err != nil {
				return PGExtPlanBody{}, err
			}
			if p == nil {
				return PGExtPlanBody{}, errs.ErrNotFound
			}
			o := PGExtPlanBody{ID: p.ID, Hash: p.Hash, Lines: orEmpty(p.Lines), Build: p.Build,
				Restart: p.Restart, PGUpdate: p.PGUpdate, Slices: orEmpty(p.Slices),
				Blockers: orEmpty(p.Blockers), Versions: make([]PGPkgChangeBody, 0, len(p.Versions))}
			for _, v := range p.Versions {
				o.Versions = append(o.Versions, PGPkgChangeBody{Name: v.Name, From: v.From, To: v.To})
			}
			return o, nil
		}
		return PGExtPlanBody{}, errs.ErrNotFound
	})
}

func orEmpty(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// ApplyPGExtPlan applies a reviewed plan by id; a stale plan is refused.
func (h *H) ApplyPGExtPlan() Endpoint {
	return JSON(200, func(c echo.Context, in PGExtApplyIn) (PGExtOut, error) {
		v, j, err := h.Orch.ApplyPGExtPlan(rc(c), middleware.Principal(c), tileID(c), in.Plan,
			service.ApproveOpts{Confirm: in.Confirm})
		return pgExtOut(v, j), err
	})
}
