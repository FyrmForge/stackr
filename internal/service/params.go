package service

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	ParamScope = params.Scope // Kind org | env | tier | stack_pr | org_pr
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
	if err := o.refuseTieredOrg(ctx, s); err != nil {
		return nil, err
	}
	return o.changeParams(ctx, s, func() error {
		return o.store.Tx(ctx, func(tx store.Tx) error { return params.New(tx.Params).Merge(ctx, s, es) })
	})
}

// DeleteParam drops one param or secret; running tiles that read it
// redeploy, and the answer names them.
func (o *Orchestrator) DeleteParam(ctx context.Context, s ParamScope, collection, name string) ([]Redeploy, error) {
	if err := o.refuseTieredOrg(ctx, s); err != nil {
		return nil, err
	}
	return o.changeParams(ctx, s, func() error { return o.params.Delete(ctx, s, collection, name) })
}

// refuseTieredOrg: a tiered org's org-scope values are read by no env, so a
// write there would be stored and never used.
func (o *Orchestrator) refuseTieredOrg(ctx context.Context, s ParamScope) error {
	if s.Kind != "org" {
		return nil
	}
	ts, err := o.tiers.List(ctx, s.ID)
	if err != nil {
		return err
	}
	if len(ts) > 0 {
		return errs.Conflictf("this org has tiers; use --tier or --pr")
	}
	return nil
}

// envTier is the tier a static env is in; a PR env is in none.
func (o *Orchestrator) envTier(ctx context.Context, e Environment) (Tier, bool, error) {
	if e.Type != environment.Static {
		return Tier{}, false, nil
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return Tier{}, false, err
	}
	return o.tiers.Of(ctx, st.OrgID, e.Slug)
}

// changeParams runs write and redeploys the scope's tiles that read a key
// whose value or kind it moved. An unchanged value restarts nothing.
func (o *Orchestrator) changeParams(ctx context.Context, s ParamScope, write func() error) ([]Redeploy, error) {
	return o.changeParamsFrom(ctx, s, "", write)
}

// changeParamsFrom is changeParams for a write made inside a job that rolls
// out env skipEnv itself (a promote or sync): that env's tiles are left to
// it and only readers elsewhere redeploy ("" = none skipped).
func (o *Orchestrator) changeParamsFrom(ctx context.Context, s ParamScope, skipEnv string, write func() error) ([]Redeploy, error) {
	ts, err := o.stageParams(ctx, s, skipEnv, write)
	if err != nil {
		return nil, err
	}
	return o.redeploy(ctx, ts)
}

// stageParams runs write and returns the tiles that read a key it moved,
// without redeploying them. A stack_pr write also copies its secrets into the
// open PR envs first, so a failed redeploy never leaves a PR env's copy stale.
func (o *Orchestrator) stageParams(ctx context.Context, s ParamScope, skipEnv string, write func() error) ([]Tile, error) {
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
		return nil, nil
	}
	ts, err := o.scopeTiles(ctx, s)
	if err != nil {
		return nil, err
	}
	if skipEnv != "" {
		ts = slices.DeleteFunc(ts, func(t Tile) bool { return t.EnvironmentID == skipEnv })
	}
	if ts, err = o.readers(ctx, s, ts, keys, false); err != nil {
		return nil, err
	}
	if s.Kind != "stack_pr" {
		return ts, nil
	}
	more, err := o.pushPRSecrets(ctx, s, before, after, keys)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, t := range ts {
		seen[t.ID] = true
	}
	for _, t := range more {
		if !seen[t.ID] {
			seen[t.ID] = true
			ts = append(ts, t)
		}
	}
	return ts, nil
}

// pushPRSecrets copies a changed secret of the stack's pr block into every
// open PR env's own scope (a deleted one is deleted there) and returns their
// readers. A plain change never reaches a PR env: it took its copy at creation.
// ponytail: copies are not one transaction; a store error midway leaves later
// PR envs on the old secret until the next stack_pr change.
func (o *Orchestrator) pushPRSecrets(ctx context.Context, s ParamScope, before, after map[string]params.Value, keys map[string]bool) ([]Tile, error) {
	es, err := o.envs.List(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	var out []Tile
	for _, e := range es {
		if e.Type != environment.Ephemeral {
			continue
		}
		sc := ParamScope{Kind: "env", ID: e.ID}
		more, err := o.stageParams(ctx, sc, "", func() error {
			for _, k := range slices.Sorted(maps.Keys(keys)) {
				c, n, _ := strings.Cut(k, ".")
				switch v, ok := after[k]; {
				case ok && v.Secret:
					if err := o.params.Merge(ctx, sc, []params.Entry{{Collection: c, Name: n, Kind: params.Secret, Value: v.V}}); err != nil {
						return err
					}
				case !ok && before[k].Secret:
					if err := o.params.Delete(ctx, sc, c, n); err != nil && !errors.Is(err, errs.ErrNotFound) {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

// seedPREnv gives a new PR env its own copy of the stack's pr block, secrets
// included.
func (o *Orchestrator) seedPREnv(ctx context.Context, e Environment) error {
	have, err := o.params.Values(ctx, ParamScope{Kind: "stack_pr", ID: e.StackID}, true)
	if err != nil {
		return err
	}
	var es []params.Entry
	for _, k := range slices.Sorted(maps.Keys(have)) {
		c, n, _ := strings.Cut(k, ".")
		kind := params.Param
		if have[k].Secret {
			kind = params.Secret
		}
		es = append(es, params.Entry{Collection: c, Name: n, Kind: kind, Value: have[k].V})
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		return params.New(tx.Params).Merge(ctx, ParamScope{Kind: "env", ID: e.ID}, es)
	})
}

// writeParams is the hook a promote or sync writes its params through, so
// readers in other envs redeploy.
func (o *Orchestrator) writeParams(ctx context.Context, s ParamScope, skipEnv string, write func() error) error {
	_, err := o.changeParamsFrom(ctx, s, skipEnv, write)
	return err
}

// reach is what a change in a scope hits: the ref kind that reads it, the
// [x] qualifier that names it from elsewhere ("" = none), which envs read it
// with no qualifier, and whether its block is locked against [x] readers.
func (o *Orchestrator) reach(ctx context.Context, s ParamScope) (kind params.Kind, qual string, plain func(Environment) bool, locked bool, err error) {
	switch s.Kind {
	case "env":
		e, err := o.envs.Get(ctx, s.ID)
		if err != nil {
			return kind, qual, plain, false, err
		}
		locked = e.Locked
		if t, in, err := o.envTier(ctx, e); err != nil {
			return kind, qual, plain, false, err
		} else if in {
			locked = t.Locked
		}
		return params.KindParam, e.Slug, func(x Environment) bool { return x.ID == e.ID }, locked, nil
	case "tier":
		t, err := o.tiers.Get(ctx, s.ID)
		return params.KindOrgParam, t.Slug, func(x Environment) bool { return x.Slug == t.Slug && x.Type != environment.Ephemeral }, t.Locked, err
	case "stack_pr":
		// a PR env keeps its own copy; only a [pr] ref reads this block
		return params.KindParam, tier.PR, func(Environment) bool { return false }, false, nil
	case "org_pr":
		return params.KindOrgParam, tier.PR, func(x Environment) bool { return x.Type == environment.Ephemeral }, false, nil
	}
	ts, err := o.tiers.List(ctx, s.ID)
	return params.KindOrgParam, "", func(x Environment) bool { return len(ts) == 0 && x.Type != environment.Ephemeral }, false, err
}

// readers keeps the tiles that read one of keys ("collection.name" in s): a
// ref in env values, volume lines or the command, a mounted share's login,
// or a slice ref whose provision_from reads it. A ref reads s plain from an
// env that s is the implicit block of (see reach), or qualified [x] from any
// tile. A :template files line is read from the config repo at deploy, so
// such a tile reads every key, when s is readable from its env (its own
// block, or an unlocked one). nil keys = every key (a lock or tier change
// moves the whole view); qualOnly drops the plain readers (a lock moves
// only what [x] refs see, and a template counts unless s is its own block).
func (o *Orchestrator) readers(ctx context.Context, s ParamScope, ts []Tile, keys map[string]bool, qualOnly bool) ([]Tile, error) {
	kind, qual, plain, locked, err := o.reach(ctx, s)
	if err != nil {
		return nil, err
	}
	envs := map[string]Environment{}
	for _, t := range ts {
		if _, ok := envs[t.EnvironmentID]; ok {
			continue
		}
		if envs[t.EnvironmentID], err = o.envs.Get(ctx, t.EnvironmentID); err != nil {
			return nil, err
		}
	}
	reads := func(t Tile, v string) bool {
		for _, b := range params.Refs(v) {
			r, err := params.Parse(b)
			if err != nil || r.Kind != kind || keys != nil && !keys[r.Slug+"."+r.Name] {
				continue
			}
			if r.Env == "" && !qualOnly && plain(envs[t.EnvironmentID]) || r.Env != "" && r.Env == qual {
				return true
			}
		}
		return false
	}
	viaSlice := map[string]bool{} // env id + slug of a slice whose provision_from reads a key
	for _, t := range ts {
		if t.Kind == tile.Slice && t.ProvisionFrom != nil && reads(t, *t.ProvisionFrom) {
			viaSlice[t.EnvironmentID+"/"+t.Slug] = true
		}
	}
	var out []Tile
	for _, t := range ts {
		own := plain(envs[t.EnvironmentID])
		tmpl := !qualOnly && (own || !locked) || qualOnly && !own
		ok, err := o.tileReads(ctx, t, func(v string) bool { return reads(t, v) }, viaSlice, tmpl)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (o *Orchestrator) tileReads(ctx context.Context, t Tile, reads func(string) bool, viaSlice map[string]bool, tmpl bool) (bool, error) {
	if reads(t.EnvJSON) || reads(t.Volumes) || reads(t.Command) {
		return true, nil
	}
	for _, l := range tile.Lines(t.Files) {
		if tmpl && strings.HasSuffix(l, ":template") {
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

// scopeTiles is every tile a scope's params may reach: an env's change
// reaches its whole stack (another env reads it as params.c[x]), a stack_pr's
// likewise; a tier, org_pr or org change reaches the whole org.
func (o *Orchestrator) scopeTiles(ctx context.Context, s ParamScope) ([]Tile, error) {
	orgID := s.ID
	switch s.Kind {
	case "stack_pr":
		return o.tiles.ListByStack(ctx, s.ID)
	case "env":
		e, err := o.envs.Get(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		return o.tiles.ListByStack(ctx, e.StackID)
	case "tier":
		t, err := o.tiers.Get(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		orgID = t.OrgID
	case "org", "org_pr":
	default:
		return nil, nil
	}
	sts, err := o.stacks.List(ctx, orgID)
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

// readersOf is the tiles whose view of scope s may have moved, every key
// (see readers).
func (o *Orchestrator) readersOf(ctx context.Context, s ParamScope, qualOnly bool) ([]Tile, error) {
	ts, err := o.scopeTiles(ctx, s)
	if err != nil {
		return nil, err
	}
	return o.readers(ctx, s, ts, nil, qualOnly)
}

// redeployUnion redeploys the running tiles of several lists, each once.
func (o *Orchestrator) redeployUnion(ctx context.Context, lists ...[]Tile) error {
	seen := map[string]bool{}
	var ts []Tile
	for _, l := range lists {
		for _, t := range l {
			if !seen[t.ID] {
				seen[t.ID] = true
				ts = append(ts, t)
			}
		}
	}
	return o.redeployRunning(ctx, ts)
}
