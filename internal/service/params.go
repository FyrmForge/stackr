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

// ServerParamScope is the server's params (admin only): read by the server
// file's items and by nothing else.
var ServerParamScope = ParamScope(params.ServerScope)

// Params lists a scope's entries. secrets=false never reads a secret row
// (B37): a caller without the right gets params only.
func (o *Orchestrator) Params(ctx context.Context, s ParamScope, secrets bool) ([]Param, error) {
	return o.params.List(ctx, s, secrets)
}

// MaskedParams lists a scope's params and its secrets with the value blank:
// names without reading a secret (B37).
func (o *Orchestrator) MaskedParams(ctx context.Context, s ParamScope) ([]Param, error) {
	return o.params.Masked(ctx, s)
}

// SetParams merges entries into a scope in one transaction: a param over a
// secret is refused before any row moves (B4), a masked secret with no value
// keeps the stored one (B35). Running tiles under the scope redeploy (B34),
// and the answer names them.
func (o *Orchestrator) SetParams(ctx context.Context, s ParamScope, es []ParamEntry) ([]Redeploy, error) {
	if err := o.store.Tx(ctx, func(tx store.Tx) error { return params.New(tx.Params).Merge(ctx, s, es) }); err != nil {
		return nil, err
	}
	ts, err := o.scopeTiles(ctx, s)
	if err != nil {
		return nil, err
	}
	return o.redeploy(ctx, ts)
}

// DeleteParam drops one param or secret; running tiles under the scope
// redeploy, and the answer names them.
func (o *Orchestrator) DeleteParam(ctx context.Context, s ParamScope, collection, name string) ([]Redeploy, error) {
	if err := o.params.Delete(ctx, s, collection, name); err != nil {
		return nil, err
	}
	ts, err := o.scopeTiles(ctx, s)
	if err != nil {
		return nil, err
	}
	return o.redeploy(ctx, ts)
}

// ExpandServerRefs resolves the ${{ server.params.<collection>.<name> }} refs
// in one item of the server file (a backup dest key, later a connector
// token). A missing value is errs.Unset naming server.params.<collection>.<name>;
// any other ref form is refused. It is the only reader of the server scope.
func (o *Orchestrator) ExpandServerRefs(ctx context.Context, s string) (string, error) {
	if !params.HasRef(s) {
		return s, nil
	}
	vals, err := o.params.Values(ctx, params.ServerScope, true)
	if err != nil {
		return "", err
	}
	return params.NewResolver(params.Snapshot{ServerParams: vals}).Expand(params.InServerFile, s)
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
