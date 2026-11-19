package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
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
// writing anything: a changed value or a secret given a new value counts, an
// unchanged one does not. A stored secret is decrypted here to compare and
// never leaves.
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
			// compared here and never returned; the real save skips an unchanged value too
			if cur, err := o.params.Secret(ctx, s, e.Collection, e.Name); e.Value != "" && (err != nil || cur.Value != e.Value) {
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

// parkedJob is a job that waits on a value: parked now, or back queued or
// running for its re-check (a parked job runs its handler again every poll,
// and its row then holds no waiting_param; the runner remembers it).
type parkedJob struct {
	Job   Job
	Param string // "collection.name", or "org.collection.name"
}

// parkedJobs are the jobs waiting on a value, in every state they cycle
// through, so a view of them never blinks while one is re-checked.
func (o *Orchestrator) parkedJobs(ctx context.Context) ([]parkedJob, error) {
	js, err := o.jobRows.List(ctx, job.Waiting, job.Queued, job.Running)
	if err != nil {
		return nil, err
	}
	var out []parkedJob
	for _, j := range js {
		p := o.jobs.ParkedParam(j.ID)
		if j.WaitingParam != nil {
			p = *j.WaitingParam
		}
		if _, approval := hostgrant.Parse(p); p == "" || approval {
			continue
		}
		out = append(out, parkedJob{j, p})
	}
	return out, nil
}

// parkedTiles are the tiles a parked job names: a deploy's tile, or what a
// parked env sync still owes. A promote names none (it locks the env's tiles,
// see lockedTiles).
func parkedTiles(j Job) []string {
	var p struct {
		TileID string   `json:"tile_id"`
		Owed   []string `json:"owed"`
	}
	_ = json.Unmarshal([]byte(j.Payload), &p)
	if p.TileID != "" {
		return []string{p.TileID}
	}
	return p.Owed
}

// lockedTiles are the tile ids in a job's lock set (not "env:<id>" or "*").
func lockedTiles(j Job) []string {
	return slices.DeleteFunc(slices.Clone([]string(j.LockSet)), func(k string) bool { return k == job.All || strings.Contains(k, ":") })
}

// stillUnset says a re-checking job's param has no value yet. Jobs that wait
// in the parked state need no check: the runner requeues them only to look again.
// ponytail: an org param (own tier or org block) is not looked up here; the
// job counts as waiting for the few seconds its deploy runs after the value lands.
func (o *Orchestrator) stillUnset(ctx context.Context, pj parkedJob, envID string) bool {
	if pj.Job.State == job.Waiting || strings.HasPrefix(pj.Param, "org.") {
		return true
	}
	have, err := o.params.Values(ctx, ParamScope{Kind: "env", ID: envID}, false)
	_, set := have[pj.Param]
	return err != nil || !set
}

// UnsetParams names the values an env's tiles wait for: a deploy parked on a
// param of this env that has no value yet, with the tiles parked on it
// ("collection.name" to tile slugs). A declared secret stores no row, so the
// parked deploys are where the declaration shows.
func (o *Orchestrator) UnsetParams(ctx context.Context, s ParamScope) (map[string][]string, error) {
	out, err := o.UnsetParamsOf(ctx, []ParamScope{s})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// UnsetParamsOf is UnsetParams for several scopes with the parked jobs read
// once (a grid asks for one per env column); the answer is in scope order.
func (o *Orchestrator) UnsetParamsOf(ctx context.Context, ss []ParamScope) ([]map[string][]string, error) {
	pjs, err := o.parkedJobs(ctx)
	if err != nil {
		return nil, err
	}
	res := make([]map[string][]string, len(ss))
	for i, s := range ss {
		if s.Kind != "env" {
			continue
		}
		have, err := o.params.Values(ctx, s, false)
		if err != nil {
			return nil, err
		}
		out := map[string][]string{}
		for _, pj := range pjs {
			k := pj.Param
			if strings.ContainsAny(k, "[:") || strings.Count(k, ".") != 1 {
				continue
			}
			if _, ok := have[k]; ok {
				continue
			}
			for _, id := range parkedTiles(pj.Job) {
				if t, err := o.tiles.Get(ctx, id); err == nil && t.EnvironmentID == s.ID && !slices.Contains(out[k], t.Slug) {
					out[k] = append(out[k], t.Slug)
				}
			}
		}
		for _, v := range out {
			slices.Sort(v)
		}
		res[i] = out
	}
	return res, nil
}

// SetParamsLater is SetParams that leaves the readers running: it names the
// tiles the change reaches and marks them redeploy pending (RestartPending)
// until a deploy that recreates them has succeeded (a plain restart does not
// apply a value). Parked deploys that the value would resume are cancelled
// first, so a tile that waits stays stopped; waiting names those tiles.
// ponytail: the mark lives in memory and is lost when the panel restarts; a
// column on tiles when that hurts.
func (o *Orchestrator) SetParamsLater(ctx context.Context, s ParamScope, es []ParamEntry) (redeploy, waiting []string, err error) {
	if err := o.refuseTieredOrg(ctx, s); err != nil {
		return nil, nil, err
	}
	// Before the write: once the value is there the runner's next poll resumes them.
	waitTiles, err := o.cancelParked(ctx, s, es)
	if err != nil {
		return nil, nil, err
	}
	ts, err := o.stageParams(ctx, s, "", func() error {
		return o.store.Tx(ctx, func(tx store.Tx) error { return params.New(tx.Params).Merge(ctx, s, es) })
	})
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.Collection + "/" + e.Name
	}
	if err = o.noted(ctx, err, securitylog.ParamSet, s.Kind, s.ID, "set "+strings.Join(names, ", ")); err != nil {
		return nil, nil, err
	}
	for _, t := range waitTiles {
		o.restartPending.Store(t.ID, struct{}{})
		waiting = append(waiting, t.Slug)
	}
	for _, t := range ts {
		if ok, err := o.reachedByRedeploy(ctx, t); err != nil {
			return nil, nil, err
		} else if ok && !slices.Contains(waiting, t.Slug) && !slices.Contains(redeploy, t.Slug) {
			o.restartPending.Store(t.ID, struct{}{})
			redeploy = append(redeploy, t.Slug)
		}
	}
	slices.Sort(redeploy)
	slices.Sort(waiting)
	return redeploy, waiting, nil
}

// cancelParked cancels the deploys parked on a value es gives s (whichever
// state the runner has them in right now), and returns the tiles that waited.
// A job that cannot be stopped (mid-swap) refuses the save.
func (o *Orchestrator) cancelParked(ctx context.Context, s ParamScope, es []ParamEntry) ([]Tile, error) {
	have, err := o.params.Values(ctx, s, false)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{} // the param text a parked job names for each value
	keys := map[string]bool{}
	for _, e := range es {
		k := e.Collection + "." + e.Name
		if _, held := have[k]; e.Value == "" || held {
			continue
		}
		keys[k] = true
		if s.Kind == "env" || s.Kind == "stack_pr" {
			want[k] = true
		} else {
			want["org."+k] = true
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	pjs, err := o.parkedJobs(ctx)
	if err != nil {
		return nil, err
	}
	pjs = slices.DeleteFunc(pjs, func(pj parkedJob) bool { return !want[pj.Param] })
	if len(pjs) == 0 {
		return nil, nil
	}
	ts, err := o.scopeTiles(ctx, s)
	if err != nil {
		return nil, err
	}
	if s.Kind == "env" {
		ts = slices.DeleteFunc(ts, func(t Tile) bool { return t.EnvironmentID != s.ID })
	}
	if ts, err = o.readers(ctx, s, ts, keys, false); err != nil {
		return nil, err
	}
	in := func(id string) bool { return slices.ContainsFunc(ts, func(t Tile) bool { return t.ID == id }) }
	var out []Tile
	for _, pj := range pjs {
		targets := parkedTiles(pj.Job)
		if len(targets) == 0 {
			targets = lockedTiles(pj.Job)
		}
		if !slices.ContainsFunc(targets, in) {
			continue
		}
		if err := o.jobs.Cancel(ctx, pj.Job.ID); err != nil && !errors.Is(err, jobs.ErrFinished) {
			return nil, errs.Conflictf("A deploy waiting for this value cannot be stopped now. Try again in a moment.")
		}
		for _, id := range parkedTiles(pj.Job) {
			if i := slices.IndexFunc(ts, func(t Tile) bool { return t.ID == id }); i >= 0 && !slices.ContainsFunc(out, func(t Tile) bool { return t.ID == id }) {
				out = append(out, ts[i])
			}
		}
	}
	return out, nil
}

// RestartPending: a saved change reaches the tile and no deploy that
// recreates it has succeeded since.
func (o *Orchestrator) RestartPending(tileID string) bool {
	_, ok := o.restartPending.Load(tileID)
	return ok
}
