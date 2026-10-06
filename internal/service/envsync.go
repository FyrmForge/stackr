package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
)

type (
	SyncPlan = promote.Sync
	SyncTile = promote.SyncTile
)

// EnvSyncPlan is the dry run of a sync and its verdict: CanDeploy is false
// when the plan is blocked or has nothing to write.
type EnvSyncPlan struct {
	Plan      *SyncPlan `json:"plan"`
	CanDeploy bool      `json:"can_deploy"`
}

// EnvSyncSources says whether an env can sync and from where.
type EnvSyncSources struct {
	Can bool
	Why string // "" when Can; else the reason, one sentence
	// Default is the source's slug: the env below on the ladder, else the
	// ladder's bottom rung, else the first other env; "" when none.
	Default string
	Envs    []Environment // every other env of the stack, ladder order first
}

// EnvSyncSources is what the Sync button and the review's picker read.
func (o *Orchestrator) EnvSyncSources(ctx context.Context, envID string) (EnvSyncSources, error) {
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return EnvSyncSources{}, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return EnvSyncSources{}, err
	}
	all, err := o.envs.List(ctx, e.StackID)
	if err != nil {
		return EnvSyncSources{}, err
	}
	var s EnvSyncSources
	for _, x := range all {
		if x.ID != e.ID {
			s.Envs = append(s.Envs, x)
		}
	}
	switch {
	case st.ConfigRepo != "":
		s.Why = "Managed by the stack file."
		return s, nil
	case len(s.Envs) == 0:
		s.Why = "The stack has one environment."
		return s, nil
	}
	s.Can = true
	if below, err := o.envs.Below(ctx, e); err == nil {
		s.Default = below.Slug
		return s, nil
	} else if !errors.Is(err, errs.ErrNotFound) {
		return s, err
	}
	ladder, err := o.envs.Ladder(ctx, e.StackID)
	if err != nil {
		return s, err
	}
	i := slices.IndexFunc(ladder, func(x Environment) bool { return x.ID != e.ID })
	if i >= 0 {
		s.Default = ladder[i].Slug
	} else {
		s.Default = s.Envs[0].Slug
	}
	return s, nil
}

// syncSource resolves the source env, by slug, inside e's stack.
func (o *Orchestrator) syncSource(ctx context.Context, envID, from string) (Environment, Environment, error) {
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return e, Environment{}, err
	}
	src, err := o.envs.GetBySlug(ctx, e.StackID, from)
	return e, src, err
}

// PlanEnvSync is the dry run of making envID match the env slugged from,
// minus the dropped tiles.
func (o *Orchestrator) PlanEnvSync(ctx context.Context, envID, from string, drop []string) (EnvSyncPlan, error) {
	e, src, err := o.syncSource(ctx, envID, from)
	if err != nil {
		return EnvSyncPlan{}, err
	}
	p, err := o.promote.SyncPlan(ctx, e.ID, src.ID, drop)
	if err != nil {
		return EnvSyncPlan{}, err
	}
	return EnvSyncPlan{Plan: p, CanDeploy: !p.Blocked() && len(p.Changes) > 0}, nil
}

// EnvSync queues the sync of the kept tiles. sig is the review's; the job
// re-plans and refuses on drift, as this does.
func (o *Orchestrator) EnvSync(ctx context.Context, envID, from string, keep []string, sig string) (Job, error) {
	e, src, err := o.syncSource(ctx, envID, from)
	if err != nil {
		return Job{}, err
	}
	p, err := o.promote.SyncPlan(ctx, e.ID, src.ID, nil)
	if err != nil {
		return Job{}, err
	}
	if p.Sig != sig {
		return Job{}, errs.Conflictf("the environments changed since the review; review again")
	}
	// Blockers of a dropped tile do not count: plan what is kept.
	var drop []string
	for _, t := range p.Tiles {
		if !slices.Contains(keep, t.Slug) {
			drop = append(drop, t.Slug)
		}
	}
	if len(drop) > 0 {
		if p, err = o.promote.SyncPlan(ctx, e.ID, src.ID, drop); err != nil {
			return Job{}, err
		}
	}
	if p.Blocked() {
		return Job{}, errs.Conflictf("%s", strings.Join(p.Blockers, "; "))
	}
	if len(p.Changes) == 0 {
		return Job{}, errs.Conflictf("nothing to sync")
	}
	ts, err := o.tiles.List(ctx, e.ID)
	if err != nil {
		return Job{}, err
	}
	lock := []string{"env:" + e.ID}
	for _, t := range ts {
		lock = append(lock, t.ID)
	}
	return o.enqueue(ctx, kindEnvSync, envSyncJob{EnvID: e.ID, FromID: src.ID, Keep: keep, Sig: sig}, lock...)
}

// EnvSyncCanvas is the env canvas with the sync overlaid: the tagged cards
// and a ghost for each tile the sync would add, and the plan behind them.
func (o *Orchestrator) EnvSyncCanvas(
	ctx context.Context,
	envID, from string,
	drop []string,
	show GraphShow,
) (GraphView, EnvSyncPlan, error) {
	pl, err := o.PlanEnvSync(ctx, envID, from, drop)
	if err != nil {
		return GraphView{}, pl, err
	}
	in, err := o.graphIn(ctx, CanvasScope{Kind: CanvasEnv, ID: envID}, show, true)
	if err != nil {
		return GraphView{}, pl, err
	}
	in.Sync = map[string]string{}
	for _, t := range pl.Plan.Tiles {
		if !t.Dropped {
			in.Sync[t.Slug] = t.Tag
		}
	}
	in.New = pl.Plan.Creates
	v, err := o.graph.Build(ctx, CanvasScope{Kind: CanvasEnv, ID: envID}, in)
	return v, pl, err
}
