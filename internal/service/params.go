package service

import (
	"context"
	"errors"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
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
// keeps the stored one (B35). Running tiles that read a changed key
// redeploy (B34), and the answer names them.
func (o *Orchestrator) SetParams(ctx context.Context, s ParamScope, es []ParamEntry) ([]Redeploy, error) {
	return o.changeParams(ctx, s, func() error {
		return o.store.Tx(ctx, func(tx store.Tx) error { return params.New(tx.Params).Merge(ctx, s, es) })
	})
}

// DeleteParam drops one param or secret; running tiles that read it
// redeploy, and the answer names them.
func (o *Orchestrator) DeleteParam(ctx context.Context, s ParamScope, collection, name string) ([]Redeploy, error) {
	return o.changeParams(ctx, s, func() error { return o.params.Delete(ctx, s, collection, name) })
}

// changeParams runs write and redeploys the scope's tiles that read a key
// whose value or kind it moved. An unchanged value restarts nothing.
func (o *Orchestrator) changeParams(ctx context.Context, s ParamScope, write func() error) ([]Redeploy, error) {
	before, err := o.params.Values(ctx, s, true)
	if err != nil {
		return nil, err
	}
	if err := write(); err != nil {
		return nil, err
	}
	after, err := o.params.Values(ctx, s, true)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for k, v := range after {
		if b, ok := before[k]; !ok || b != v {
			keys[k] = true
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			keys[k] = true
		}
	}
	if len(keys) == 0 {
		return []Redeploy{}, nil
	}
	ts, err := o.scopeTiles(ctx, s)
	if err != nil {
		return nil, err
	}
	if ts, err = o.readers(ctx, s, ts, keys); err != nil {
		return nil, err
	}
	return o.redeploy(ctx, ts)
}

// readers keeps the tiles that read one of keys ("collection.name" in s): a
// ref in env values, volume lines or the command, a mounted share's login,
// or a slice ref whose provision_from reads it. A :template files line is
// read from the config repo at deploy, so such a tile reads every key.
func (o *Orchestrator) readers(ctx context.Context, s ParamScope, ts []Tile, keys map[string]bool) ([]Tile, error) {
	kind := params.KindParam
	if s.Kind == "org" {
		kind = params.KindOrgParam
	}
	reads := func(v string) bool {
		for _, b := range params.Refs(v) {
			if r, err := params.Parse(b); err == nil && r.Kind == kind && keys[r.Slug+"."+r.Name] {
				return true
			}
		}
		return false
	}
	viaSlice := map[string]bool{} // env id + slug of a slice whose provision_from reads a key
	for _, t := range ts {
		if t.Kind == tile.Slice && t.ProvisionFrom != nil && reads(*t.ProvisionFrom) {
			viaSlice[t.EnvironmentID+"/"+t.Slug] = true
		}
	}
	var out []Tile
	for _, t := range ts {
		ok, err := o.tileReads(ctx, t, reads, viaSlice)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (o *Orchestrator) tileReads(ctx context.Context, t Tile, reads func(string) bool, viaSlice map[string]bool) (bool, error) {
	if reads(t.EnvJSON) || reads(t.Volumes) || reads(t.Command) {
		return true, nil
	}
	for _, l := range tile.Lines(t.Files) {
		if strings.HasSuffix(l, ":template") {
			return true, nil
		}
	}
	for _, b := range params.Refs(t.EnvJSON + "\n" + t.Volumes + "\n" + t.Command) {
		if r, err := params.Parse(b); err == nil && r.Kind == params.KindTile && viaSlice[t.EnvironmentID+"/"+r.Slug] {
			return true, nil
		}
	}
	for _, l := range tile.Lines(t.Volumes) {
		m, err := tile.ParseMount(l)
		if err != nil || m.Kind != tile.MountShare {
			continue
		}
		st, err := o.stacks.Get(ctx, t.StackID)
		if err != nil {
			return false, err
		}
		sh, err := o.volumes.ShareBySlug(ctx, st.OrgID, m.Share)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if reads(sh.User) || reads(sh.PasswordRef) {
			return true, nil
		}
	}
	return false, nil
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
