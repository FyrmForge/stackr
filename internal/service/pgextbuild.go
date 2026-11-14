package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Postgres recipes on operated instances (Stage 2): the plan of an enable, the
// recipe list, the apply bound to a reviewed plan (R21). The seam holds the
// types and stubs; lane B2 owns the bodies.

// errPGBuildNotBuilt is what a stub of the recipe build returns until lane B2
// builds its body.
var errPGBuildNotBuilt = errors.New("postgres recipes are not built yet")

// pgextplanKey is an instance's last recipe plan: pgextplan.<instance id>,
// JSON of PGExtPlan.
const pgextplanKey = "pgextplan."

// PGPkgChange is one package's version change in a plan: From "" is a new
// package, To "" a removed one.
type PGPkgChange struct {
	Name, From, To string
}

// PGExtPlan is the reviewed plan of a recipe change (R21): its id and a hash
// over its inputs and the instance's base state (flow/operated.PlanHash);
// apply must name the id and is refused when the hash no longer matches
// (message 28). Lines are the rendered plan; Build says an image is built,
// Restart that the instance restarts; Versions are the package changes against
// ext_build.packages; PGUpdate is the Postgres update line ("" = none); Slices
// are the slices that list a changed extension; Blockers refuse the apply.
type PGExtPlan struct {
	ID, Hash string
	Lines    []string
	Build    bool
	Restart  bool
	Versions []PGPkgChange
	PGUpdate string
	Slices   []string
	Blockers []string
}

// PGRecipeView is a recipe as the admin page, API and CLI show it: a built-in
// or an admin-added one, switched on or off, and who may use it (Share is
// ["all"] or org ids).
type PGRecipeView struct {
	Name       string
	Builtin    bool
	Enabled    bool
	Majors     []int
	Packages   []string
	PackageURL string
	SHA256     map[string]string
	Version    string
	Extensions []string
	Preload    []string
	Requires   []string
	Share      []string
}

// PGRecipes lists the built-in and the admin-added recipes.
func (o *Orchestrator) PGRecipes(ctx context.Context) ([]PGRecipeView, error) {
	return nil, errPGBuildNotBuilt
}

// SetPGRecipe adds or rewrites an admin recipe, or switches a built-in on or
// off.
func (o *Orchestrator) SetPGRecipe(ctx context.Context, by *Principal, v PGRecipeView) (PGRecipeView, error) {
	return PGRecipeView{}, errPGBuildNotBuilt
}

// DeletePGRecipe removes an admin recipe, or a built-in's switch-off row.
func (o *Orchestrator) DeletePGRecipe(ctx context.Context, by *Principal, name string) error {
	return errPGBuildNotBuilt
}

// PlanPGExtensions queues the plan job of a recipe change of an instance and
// returns it; the plan lands under pgextplan.<instance id>.
func (o *Orchestrator) PlanPGExtensions(ctx context.Context, by *Principal, tileID string, in PGExtIn) (Job, error) {
	return Job{}, errPGBuildNotBuilt
}

// LastPGExtPlan is the instance's stored plan, nil when it has none (V10:
// exported so handlers and the drawer read it).
func (o *Orchestrator) LastPGExtPlan(ctx context.Context, instanceID string) (*PGExtPlan, error) {
	v, ok, err := o.store.Settings.Get(ctx, pgextplanKey+instanceID)
	if err != nil || !ok {
		return nil, err
	}
	var p PGExtPlan
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		return nil, errs.Conflictf("the stored plan of %s is unreadable; plan again.", instanceID)
	}
	return &p, nil
}

// ApplyPGExtPlan applies the plan planID of the instance on tileID. It
// recomputes the hash from live state; a mismatch or a missing plan is message
// 28 and queues a new plan. A plan that restarts needs opts.Confirm (the house
// approve contract) and is refused, not replanned, without it (V6).
func (o *Orchestrator) ApplyPGExtPlan(ctx context.Context, by *Principal, tileID, planID string, opts ApproveOpts) (PGExtView, *Job, error) {
	return PGExtView{}, nil, errPGBuildNotBuilt
}

// rebuildOperated is deploy's RebuildOperated hook: it brings back the image
// of a recipe-built tile from the saved image in the backup destination, else
// by building the recorded pins again (message 33 when they no longer
// install). Stub: nothing to rebuild, so the deploy fails with its pull error
// as before.
func (o *Orchestrator) rebuildOperated(ctx context.Context, t store.Tile, log io.Writer) error {
	return docker.ErrNoClusterImage
}

// pgExtPlanFromFile and pgExtApplyFromFile are promote's PGExtPlan and
// PGExtApply hooks: a stack file's recipe change is planned in the plan step
// and applied with the plan id it stored (G2 calls them).
func (o *Orchestrator) pgExtPlanFromFile(ctx context.Context, t store.Tile, m store.OperatedInstance, exts []string, log io.Writer) ([]string, string, error) {
	return nil, "", errPGBuildNotBuilt
}

func (o *Orchestrator) pgExtApplyFromFile(ctx context.Context, t store.Tile, planID string, log io.Writer) error {
	return errPGBuildNotBuilt
}
