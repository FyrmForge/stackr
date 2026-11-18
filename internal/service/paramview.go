package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// RevealParam is one secret's value, for a writer of its scope. It reads that
// one row only, and records who and which name (never the value) before the
// value is returned: when the audit row cannot be written, no value leaves.
func (o *Orchestrator) RevealParam(ctx context.Context, by *Principal, s ParamScope, collection, name string) (string, error) {
	p, err := o.params.Secret(ctx, s, collection, name)
	if err != nil {
		return "", err
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.SecretReveal, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: s.Kind, TargetID: s.ID, Detail: collection + "." + name,
		})
	})
	if err != nil {
		return "", err
	}
	return p.Value, nil
}

// ParamImpact is the tiles (by slug) a save of es would redeploy, without
// writing anything: a changed plain value or a secret given a value counts,
// an unchanged plain one does not. Secrets are never decrypted to compare.
func (o *Orchestrator) ParamImpact(ctx context.Context, s ParamScope, es []ParamEntry) ([]string, error) {
	have, err := o.params.Values(ctx, s, false)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, e := range es {
		k := e.Collection + "." + e.Name
		switch b, ok := have[k]; {
		case e.Kind == params.Secret:
			if e.Value != "" {
				keys[k] = true
			}
		case !ok || b.V != e.Value:
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
	if ts, err = o.readers(ctx, s, ts, keys, false); err != nil {
		return nil, err
	}
	var out []string
	for _, t := range ts {
		if ok, err := o.reachedByRedeploy(ctx, t); err != nil {
			return nil, err
		} else if ok && !slices.Contains(out, t.Slug) {
			out = append(out, t.Slug)
		}
	}
	slices.Sort(out)
	return out, nil
}

// UnsetParams names the values an env's tiles wait for: a deploy parked on a
// param of this env that has no value yet, with the tiles parked on it
// ("collection.name" to tile slugs). A declared secret stores no row, so the
// parked deploys are where the declaration shows.
func (o *Orchestrator) UnsetParams(ctx context.Context, s ParamScope) (map[string][]string, error) {
	if s.Kind != "env" {
		return nil, nil
	}
	have, err := o.params.Values(ctx, s, false)
	if err != nil {
		return nil, err
	}
	js, err := o.jobRows.Recent(ctx, 0, job.Waiting)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, j := range js {
		if j.WaitingParam == nil || strings.ContainsAny(*j.WaitingParam, "[:") || strings.Count(*j.WaitingParam, ".") != 1 {
			continue
		}
		var p tileJob
		if json.Unmarshal([]byte(j.Payload), &p) != nil || p.TileID == "" {
			continue
		}
		t, err := o.tiles.Get(ctx, p.TileID)
		if err != nil || t.EnvironmentID != s.ID {
			continue
		}
		k := *j.WaitingParam
		if _, ok := have[k]; !ok && !slices.Contains(out[k], t.Slug) {
			out[k] = append(out[k], t.Slug)
		}
	}
	for _, v := range out {
		slices.Sort(v)
	}
	return out, nil
}

// SetParamsLater is SetParams that leaves the readers running: it names the
// tiles the change reaches and marks them redeploy pending (RestartPending)
// until a deploy that recreates them has succeeded (a plain restart does not apply a value). Parked deploys that the value
// would resume are cancelled, so a tile that waits stays stopped.
// ponytail: the mark lives in memory and is lost when the panel restarts; a
// column on tiles when that hurts.
func (o *Orchestrator) SetParamsLater(ctx context.Context, s ParamScope, es []ParamEntry) ([]string, error) {
	if err := o.refuseTieredOrg(ctx, s); err != nil {
		return nil, err
	}
	waiting, err := o.waitingJobs(ctx, s, es)
	if err != nil {
		return nil, err
	}
	ts, err := o.stageParams(ctx, s, "", func() error {
		return o.store.Tx(ctx, func(tx store.Tx) error { return params.New(tx.Params).Merge(ctx, s, es) })
	})
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.Collection + "/" + e.Name
	}
	if err = o.noted(ctx, err, securitylog.ParamSet, s.Kind, s.ID, "set "+strings.Join(names, ", ")); err != nil {
		return nil, err
	}
	for _, id := range waiting {
		_ = o.jobs.Cancel(ctx, id)
	}
	var out []string
	for _, t := range ts {
		if ok, err := o.reachedByRedeploy(ctx, t); err != nil {
			return nil, err
		} else if ok {
			o.restartPending.Store(t.ID, struct{}{})
			out = append(out, t.Slug)
		}
	}
	slices.Sort(out)
	return out, nil
}

// waitingJobs are the parked deploys that a write of es to s would resume.
func (o *Orchestrator) waitingJobs(ctx context.Context, s ParamScope, es []ParamEntry) ([]string, error) {
	if s.Kind != "env" {
		return nil, nil
	}
	js, err := o.jobRows.Recent(ctx, 0, job.Waiting)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, j := range js {
		if j.WaitingParam == nil || !slices.ContainsFunc(es, func(e ParamEntry) bool {
			return e.Value != "" && e.Collection+"."+e.Name == *j.WaitingParam
		}) {
			continue
		}
		var p tileJob
		if json.Unmarshal([]byte(j.Payload), &p) != nil {
			continue
		}
		if t, err := o.tiles.Get(ctx, p.TileID); err == nil && t.EnvironmentID == s.ID {
			ids = append(ids, j.ID)
		}
	}
	return ids, nil
}

// RestartPending: a saved change reaches the tile and no deploy that
// recreates it has succeeded since.
func (o *Orchestrator) RestartPending(tileID string) bool {
	_, ok := o.restartPending.Load(tileID)
	return ok
}
