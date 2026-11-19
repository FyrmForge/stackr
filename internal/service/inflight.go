package service

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// jobSnap holds the live-job reads one render makes more than once (a graph
// asks per stack card and per never-deployed tile): the parked jobs, indexed
// by env, and each stack's push rows. It lives on the context a caller marks
// with WithJobSnapshot; a read without one goes to the store each time. It
// is a snapshot of one request, never kept across requests.
type jobSnap struct {
	mu     sync.Mutex
	parked []parkedAt // nil until read
	read   bool
	pushes map[string][]Job
}

type jobSnapKey struct{}

// WithJobSnapshot makes ParkedOn, PushState and Building read the job rows
// once per stack under ctx.
func WithJobSnapshot(ctx context.Context) context.Context {
	if ctx.Value(jobSnapKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, jobSnapKey{}, &jobSnap{pushes: map[string][]Job{}})
}

// parkedAt is a parked value placed at its env and stack.
type parkedAt struct {
	EnvID, StackID string
	ParkedOn
}

// parkedIndex is every value a job waits for, with its env and stack.
func (o *Orchestrator) parkedIndex(ctx context.Context) []parkedAt {
	snap, _ := ctx.Value(jobSnapKey{}).(*jobSnap)
	if snap != nil {
		snap.mu.Lock()
		defer snap.mu.Unlock()
		if snap.read {
			return snap.parked
		}
	}
	pjs, err := o.parkedJobs(ctx)
	if err != nil {
		return nil
	}
	var out []parkedAt
	for _, pj := range pjs {
		var m map[string]any
		_ = json.Unmarshal([]byte(pj.Job.Payload), &m)
		id := o.jobEnv(ctx, m)
		if id == "" {
			continue
		}
		e, err := o.envs.Get(ctx, id)
		if err != nil {
			continue
		}
		st, err := o.stacks.Get(ctx, e.StackID)
		if err != nil {
			continue
		}
		og, err := o.orgs.Get(ctx, st.OrgID)
		if err != nil {
			continue
		}
		href := "/" + og.Slug + "/" + st.Slug + "/" + e.Slug + "?drawer=env%3A" + e.ID + "&tab=variables"
		if len(pj.Param) > 4 && pj.Param[:4] == "org." {
			href = "/" + og.Slug + "?drawer=org%3A" + og.ID + "&tab=variables"
		}
		out = append(out, parkedAt{e.ID, st.ID, ParkedOn{JobID: pj.Job.ID, Param: pj.Param, Href: href}})
	}
	if snap != nil {
		snap.parked, snap.read = out, true
	}
	return out
}

// Building is a push job of a stack that is queued or running: the build
// its release (or its failure) will come from. One verb, so every place the
// result will show (stack card, env page, Releases tab, the releases view's
// "Building" row, the empty stack page) says the same thing.
type Building struct {
	JobID   string
	Sha     string // the pushed commit, 7 characters
	Started time.Time
	Queued  bool // not started yet: Started is when it was queued
}

// PushState is a stack's newest push that says something: queued, running,
// failed (Error) or done. Skipped is why a done push made no env the stack
// file names ("prod: the file gives it no branch or promote").
type PushState struct {
	Building
	Failed  bool
	Done    bool
	Error   string
	Skipped []string
	Waits   string // a parked push: what it waits for; it is not building
}

// Building is the stack's live push job; running wins over a queued one.
// ponytail: it is shown on every env of the stack, a push may build for one
// branch only; narrow it by the envs' branches when that misleads.
func (o *Orchestrator) Building(ctx context.Context, stackID string) (Building, bool) {
	p, ok := o.PushState(ctx, stackID)
	if !ok || p.Failed || p.Done || p.Waits != "" {
		return Building{}, false
	}
	return p.Building, true
}

// PushState is the stack's newest push job that is live, failed or done. A
// cancelled, superseded or skipped one says nothing.
func (o *Orchestrator) PushState(ctx context.Context, stackID string) (PushState, bool) {
	js, err := o.pushRows(ctx, stackID)
	if err != nil {
		return PushState{}, false
	}
	// a running push is the build in flight, even when a newer one queues behind it
	if i := slices.IndexFunc(js, func(j Job) bool { return j.State == job.Running }); i >= 0 {
		return pushState(js[i]), true
	}
	for _, j := range js {
		switch j.State {
		case job.Superseded, job.Cancelled, job.Skipped:
			continue
		}
		return pushState(j), true
	}
	return PushState{}, false
}

func pushState(j Job) PushState {
	var p pushJob
	_ = json.Unmarshal([]byte(j.Payload), &p)
	var m struct {
		Skipped []string `json:"skipped"`
	}
	_ = json.Unmarshal([]byte(j.Payload), &m)
	b := Building{JobID: j.ID, Sha: p.Event.Commit, Started: j.CreatedAt, Queued: j.State == job.Queued}
	if len(b.Sha) > 7 {
		b.Sha = b.Sha[:7]
	}
	if j.StartedAt != nil && j.State == job.Running {
		b.Started = *j.StartedAt
	}
	waits := ""
	if j.State == job.Waiting {
		waits = "Waiting"
		if j.WaitingParam != nil {
			waits = "Waits for " + *j.WaitingParam
		}
	}
	return PushState{Building: b, Waits: waits, Failed: j.State == job.Failed, Done: j.State == job.Done, Error: j.Error, Skipped: m.Skipped}
}

// ParkedOn is a value a job of the stack waits for, and where to set it.
type ParkedOn struct{ JobID, Param, Href string }

// ParkedOn lists the values the stack's parked jobs wait for, one per value:
// of one env, or of any env of the stack when envID is "". Waiting on a
// server admin's approval is not a value and is not listed.
func (o *Orchestrator) ParkedOn(ctx context.Context, stackID, envID string) []ParkedOn {
	var out []ParkedOn
	for _, p := range o.parkedIndex(ctx) {
		if p.StackID != stackID || (envID != "" && p.EnvID != envID) || slices.ContainsFunc(out, func(x ParkedOn) bool { return x.Param == p.Param }) {
			continue
		}
		out = append(out, p.ParkedOn)
	}
	return out
}

// pushRows is the stack's newest push jobs, newest first, read once per
// WithJobSnapshot context.
func (o *Orchestrator) pushRows(ctx context.Context, stackID string) ([]Job, error) {
	snap, _ := ctx.Value(jobSnapKey{}).(*jobSnap)
	if snap != nil {
		snap.mu.Lock()
		defer snap.mu.Unlock()
		if js, ok := snap.pushes[stackID]; ok {
			return js, nil
		}
	}
	js, err := o.store.Jobs.ListTouching(ctx, []string{"push:" + stackID}, string(kindPush), 10)
	if err == nil && snap != nil {
		snap.pushes[stackID] = js
	}
	return js, err
}
