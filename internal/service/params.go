package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	ParamScope = params.Scope // Kind org | stack | env
	ParamEntry = params.Entry
	Param      = store.Param
)

// Params lists a scope's entries. secrets=false never reads a secret row
// (B37): a caller without the right gets params only.
func (o *Orchestrator) Params(ctx context.Context, s ParamScope, secrets bool) ([]Param, error) {
	return o.params.List(ctx, s, secrets)
}

// SetParams merges entries into a scope in one transaction: a param over a
// secret is refused before any row moves (B4), a masked secret with no value
// keeps the stored one (B35). Running tiles under the scope redeploy (B34).
func (o *Orchestrator) SetParams(ctx context.Context, s ParamScope, es []ParamEntry) error {
	if err := o.store.Tx(ctx, func(tx store.Tx) error { return params.New(tx.Params).Merge(ctx, s, es) }); err != nil {
		return err
	}
	return o.redeployScope(ctx, s)
}

func (o *Orchestrator) DeleteParam(ctx context.Context, s ParamScope, collection, name string) error {
	if err := o.params.Delete(ctx, s, collection, name); err != nil {
		return err
	}
	return o.redeployScope(ctx, s)
}

// redeployScope redeploys the running tiles a scope's params reach.
func (o *Orchestrator) redeployScope(ctx context.Context, s ParamScope) error {
	ts, err := o.scopeTiles(ctx, s)
	if err != nil {
		return err
	}
	return o.redeployRunning(ctx, ts)
}

// scopeTiles is every tile under an env, a stack or an org.
func (o *Orchestrator) scopeTiles(ctx context.Context, s ParamScope) ([]Tile, error) {
	switch s.Kind {
	case "env":
		return o.tiles.List(ctx, s.ID)
	case "stack":
		return o.tiles.ListByStack(ctx, s.ID)
	case "org":
		sts, err := o.stacks.List(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		var ts []Tile
		for _, st := range sts {
			more, err := o.tiles.ListByStack(ctx, st.ID)
			if err != nil {
				return nil, err
			}
			ts = append(ts, more...)
		}
		return ts, nil
	}
	return nil, nil
}
