package service

import "context"

// Move all to disk: plans, groups and enqueues volume-to-disk jobs (see
// volumemove.go). It has no job of its own. Work package 2N fills these in;
// until then both verbs refuse.

// MoveAllPlan is what the Move all dialog shows before the confirm.
type MoveAllPlan struct{}

// PlanMoveAllToDisk is read only (route verb node.manage).
func (o *Orchestrator) PlanMoveAllToDisk(ctx context.Context, by *Principal, nodeID string) (MoveAllPlan, error) {
	return MoveAllPlan{}, notBuilt()
}

// MoveAllToDisk enqueues one volume-to-disk job per group (route verb node.manage).
func (o *Orchestrator) MoveAllToDisk(ctx context.Context, by *Principal, nodeID string) ([]Job, error) {
	return nil, notBuilt()
}
