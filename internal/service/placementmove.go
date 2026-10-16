package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

// Placement moves: repointing a placement moves every env on it to the new
// node, after CI-style checks; a pin change that moves an existing env goes
// through the same flow.

// MoveEnvLine is one env a move takes along.
type MoveEnvLine struct {
	Path     string
	Volumes  int
	Bytes    int64  // -1 = unknown
	Downtime string // "about 20 s"
}

// PlacementMovePlan is the checks and impact of a move before it runs.
type PlacementMovePlan struct {
	Placement, From, To string // node names (admin) or placement names (env pin change)
	Restore             bool   // the source was forgotten: restore from backups
	Envs                []MoveEnvLine
	Checks              []planfile.Check
	OK                  bool
}

// PlacementMoveStatus is a running or finished repoint.
type PlacementMoveStatus struct {
	State     string // running | failed | done
	From, To  string
	Done, All int
	Error     string
	Envs      []MoveStatus
}

// MoveNeeded is SetPin's refusal when the pin change moves an env.
type MoveNeeded struct{ Plan PlacementMovePlan }

func (e *MoveNeeded) Error() string {
	return "this pin change moves " + e.Plan.Placement + "; confirm the move"
}

func (o *Orchestrator) moveChecks(ctx context.Context, envIDs []string, fromNode, toNode string) ([]planfile.Check, error) { //nolint:unused // lane M
	return nil, errs.ErrNotBuilt
}

// PlanPlacementMove is the checks and envs of repointing a placement (admin).
func (o *Orchestrator) PlanPlacementMove(ctx context.Context, placement, toNode string) (PlacementMovePlan, error) {
	return PlacementMovePlan{}, errs.ErrNotBuilt
}

// MovePlacement (admin) repoints a placement: confirm is its name. The job
// is not awaited.
func (o *Orchestrator) MovePlacement(ctx context.Context, by *Principal, placement, toNode, confirm string) (Job, error) {
	return Job{}, errs.ErrNotBuilt
}

// RetryPlacementMove (admin) moves the envs a failed repoint left behind.
func (o *Orchestrator) RetryPlacementMove(ctx context.Context, by *Principal, placement string) (Job, error) {
	return Job{}, errs.ErrNotBuilt
}

// PlacementMove is the placement's latest move; nil = none.
func (o *Orchestrator) PlacementMove(ctx context.Context, placement string) (*PlacementMoveStatus, error) {
	return nil, errs.ErrNotBuilt
}

// PlanEnvPin is the move pinning an env to placement would cause.
func (o *Orchestrator) PlanEnvPin(ctx context.Context, envID, placement string) (PlacementMovePlan, error) {
	return PlacementMovePlan{}, errs.ErrNotBuilt
}

// MoveEnvToPlacement pins an env and moves it: confirm is the env slug.
func (o *Orchestrator) MoveEnvToPlacement(ctx context.Context, by *Principal, envID, placement, confirm string) (Job, error) {
	return Job{}, errs.ErrNotBuilt
}

// placementNodeSet gives a nodeless placement a node and deploys its waiting envs (D6).
func (o *Orchestrator) placementNodeSet(ctx context.Context, by *Principal, placementID, nodeID string) error { //nolint:unused // lane M
	return errs.ErrNotBuilt
}

// restorePlacement is a repoint off a forgotten node (D8): MovePlacement
// calls it when the source is in forgotten_nodes. Each env is rehomed,
// deployed and restored from backups.
func (o *Orchestrator) restorePlacement(ctx context.Context, by *Principal, placementID, toNodeID string) (Job, error) { //nolint:unused // lane M
	return Job{}, errs.ErrNotBuilt
}
