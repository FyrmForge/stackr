package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Window row kinds and release states (releases view RFC, section 4).
const (
	RowRelease  = "release"
	RowFailed   = "failed"   // a push that failed: no release was cut
	RowBuilding = "building" // a queued or running push: its release is not cut yet
	RowPlan     = "plan"     // a config plan of the org file or the server file (window and screen)

	StateWaiting    = "waiting"
	StateDeploying  = "deploying"
	StateRunning    = "running"
	StateFailed     = "failed"
	StateSuperseded = "superseded"
	StateUndeployed = "not deployed"
	StateApplied    = "applied" // a config plan
	StateRejected   = "rejected"
)

// WindowReview is one env's way into a release's plan.
type WindowReview struct {
	EnvID, Env, EnvSlug string
	Rollback            bool
}

// WindowJob is a job of a release, for its open row.
type WindowJob struct {
	ID, Kind, State string
	Took            time.Duration
}

// WindowRow is one line of the releases window: a release, a failed push or
// a push being built.
type WindowRow struct {
	Kind    string
	ID      string // the release
	Number  int    // 0 = none (a failed push, a building one, a plan)
	StackID string // the stack a release row belongs to
	PlanID  string // RowPlan: the plan
	Commit  string
	Message string
	By      string // Release.CreatedBy
	At      time.Time
	State   string
	Runs    []string // env ids that run it
	FailEnv string   // StateFailed from a promote: the env it stopped in
	// FailEnvID is that env's id: a release that failed in two envs is one
	// row for each.
	FailEnvID string
	// WaitEnvs are the envs for which this release is the newest waiting one
	// (the actionable one); only these count as "waiting".
	WaitEnvs []string
	Newest   bool
	// Lacks is the number of a waiting release this one does not hold; 0 = none.
	Lacks   int
	Reviews []WindowReview
	JobID   string // the job log of a failure or a build
	Jobs    []WindowJob
	Err     string // the error of a failed push
	Queued  bool   // building: not started yet
	// CommitURL is the commit on the git host ("" = unknown).
	CommitURL string
	// Fixed marks a failure a later push or promote of its scope cured; only
	// the Releases screen's history lists it. FixedBy is that release's
	// number, 0 = it cut none.
	Fixed   bool
	FixedBy int
}

// ReleaseWindow is the rows and counts of the releases window of a stack
// (envID "") or one of its envs. Rows are what needs a person first (a
// build, failures, waiting), then what each env runs, then a few older ones.
type ReleaseWindow struct {
	Rows            []WindowRow
	Waiting, Failed int
	Stack           string // the stack's name
	Slug            string // and its slug
	LogEnv          string // the slug of an env whose address holds the job logs
	Envs            []WindowEnv
	Bound           bool // a config window: its file is bound
}

// WindowEnv is one env of the stack in ladder order.
type WindowEnv struct{ ID, Name, Slug, Color string }

// windowOlder is how many releases beyond what waits, fails or runs the
// window lists.
const windowOlder = 3

// ReleaseWindow reads the rows from releases, the ladder, the stack's jobs
// and the failure markers the canvas already shows.
func (o *Orchestrator) ReleaseWindow(ctx context.Context, stackID, envID string) (ReleaseWindow, error) {
	return o.releaseWindow(ctx, stackID, envID, false, nil)
}

// ReleaseHistory is ReleaseWindow with every release and the failures fixed
// since, for the lists that are the record (the Releases screen, the API).
func (o *Orchestrator) ReleaseHistory(ctx context.Context, stackID, envID string) (ReleaseWindow, error) {
	return o.releaseWindow(ctx, stackID, envID, true, nil)
}

// releaseWindow is ReleaseWindow; history lists every release and the
// failures that were fixed since (the Releases screen's record), where the
// window keeps a few older ones and drops a fixed failure.
// jobs are the stack's job rows when the caller already holds them (the
// screen reads them once for every stack); nil = read them here.
func (o *Orchestrator) releaseWindow(ctx context.Context, stackID, envID string, history bool, jobs *[]Job) (ReleaseWindow, error) {
	return o.releaseWindowOf(ctx, stackID, envID, history, jobs, nil)
}

// releaseWindowOf is releaseWindow with the stack's releases too (newest
// first) when the caller holds them; nil = read them here.
func (o *Orchestrator) releaseWindowOf(ctx context.Context, stackID, envID string, history bool, jobs *[]Job, held []store.Release) (ReleaseWindow, error) {
	var w ReleaseWindow
	var envs = map[string]string{} // env id -> name
	slugs := map[string]string{}   // env id -> slug
	st, err := o.stacks.Get(ctx, stackID)
	if err != nil {
		return w, err
	}
	w.Stack, w.Slug = st.Name, st.Slug
	rs := held
	if rs == nil {
		if rs, err = o.releases.List(ctx, stackID); err != nil {
			return w, err
		}
	}
	rungs, err := o.graph.Rungs(ctx, stackID)
	if err != nil {
		return w, err
	}
	var (
		runs     = map[int][]string{} // release number -> env ids
		scoped   []string
		waitFor  = map[int][]string{} // release number -> env ids it waits for
		maxRun   int
		runsByEn = map[string]int{}
	)
	for _, r := range rungs {
		envs[r.EnvID], slugs[r.EnvID] = r.Name, r.Slug
		if w.LogEnv == "" {
			w.LogEnv = r.Slug
		}
		w.Envs = append(w.Envs, WindowEnv{ID: r.EnvID, Name: r.Name, Slug: r.Slug, Color: r.Color})
		runsByEn[r.EnvID] = r.Release
		if r.Release > 0 {
			runs[r.Release] = append(runs[r.Release], r.EnvID)
			maxRun = max(maxRun, r.Release)
		}
		if envID == "" || r.EnvID == envID {
			scoped = append(scoped, r.EnvID)
			if r.Behind {
				waitFor[r.Waiting] = append(waitFor[r.Waiting], r.EnvID)
			}
		}
	}
	// Every release cut for an env's line that holds what it runs waits for
	// it (the leaf's Newest and Between say which): its path back to what
	// the env runs, to tell which release holds which.
	ladder, err := o.envs.Ladder(ctx, stackID)
	if err != nil {
		return w, err
	}
	var (
		newest = map[string]bool{}            // release id -> the head of a line some env waits on
		paths  = map[string]map[string]bool{} // env id + release id -> ids it holds
		sets   = map[string][]store.Release{} // env id -> the releases waiting for it
	)
	// ponytail: scans the stack's job rows (ScopeJobs does); a SQL filter on
	// release_id when an install holds enough jobs to feel it.
	var js []Job
	if jobs != nil {
		js = *jobs
	} else if js, err = o.ScopeJobs(ctx, st.OrgID, stackID, "", 0); err != nil {
		return w, err
	}
	byID := make(map[string]store.Release, len(rs)) // the lineage, read once
	for _, r := range rs {
		byID[r.ID] = r
	}
	heads := map[string]string{}  // env id -> the release it runs
	headOf := map[string]string{} // env id -> the newest release waiting for it
	for _, e := range ladder {
		if e.ReleaseID != nil {
			heads[e.ID] = *e.ReleaseID
		}
	}
	for _, e := range ladder {
		if envID != "" && e.ID != envID {
			continue
		}
		runsID := ""
		if e.ReleaseID != nil {
			runsID = *e.ReleaseID
		}
		lines := release.Lines(e)
		head, err := o.releases.NewestIn(ctx, rs, runsID, lines...)
		if err != nil {
			return w, err
		}
		if head == runsID {
			continue
		}
		newest[head] = true
		headOf[e.ID] = head
		for _, r := range rs {
			if r.Line == nil || !slices.Contains(lines, *r.Line) || r.ID == runsID || r.Number <= runsByEn[e.ID] {
				continue
			}
			path, ok := release.Chain(byID, r, runsID)
			if !ok || len(path) == 0 {
				continue
			}
			sets[e.ID] = append(sets[e.ID], r)
			held := map[string]bool{}
			for _, p := range path {
				held[p.ID] = true
			}
			paths[e.ID+" "+r.ID] = held
			if !slices.Contains(waitFor[r.Number], e.ID) {
				waitFor[r.Number] = append(waitFor[r.Number], e.ID)
			}
		}
	}
	// A waiting release that leaves out a lower-numbered waiting one says so:
	// promoting it alone loses what the other cut.
	lacks := map[string]int{}
	for env, set := range sets {
		for _, r := range set {
			for _, x := range set {
				if x.Number < r.Number && x.Number > lacks[r.ID] && !paths[env+" "+r.ID][x.ID] && !paths[env+" "+x.ID][r.ID] {
					lacks[r.ID] = x.Number
				}
			}
		}
	}
	// failed promotes: one per (release, env), each with its own error
	type promoteFail struct {
		env, job, err string
		at            time.Time
	}
	failed := map[int][]promoteFail{}
	for _, id := range scoped {
		var s RolloutStop
		var ok bool
		if jobs != nil { // the newest promotes of the env out of the jobs held
			var mine []Job
			for _, j := range js {
				if j.Kind == string(kindPromote) && slices.Contains(j.LockSet, "env:"+id) {
					if mine = append(mine, j); len(mine) == 10 {
						break
					}
				}
			}
			s, ok = o.rolloutStop(ctx, id, mine)
		} else {
			s, ok = o.RolloutStop(ctx, id)
		}
		if ok && s.Release > 0 {
			failed[s.Release] = append(failed[s.Release], promoteFail{id, s.JobID, s.Error, s.At})
		}
	}
	var building []WindowRow
	deploying := map[string]bool{}
	promoted := map[string]bool{} // env id + " " + release id the env was once promoted to
	for _, j := range js {
		if j.State != "done" || j.Kind != string(kindPromote) || j.ReleaseID == nil {
			continue
		}
		for _, l := range j.LockSet {
			if env, ok := strings.CutPrefix(l, "env:"); ok {
				promoted[env+" "+*j.ReleaseID] = true
			}
		}
	}
	if b, ok := o.Building(ctx, stackID); ok { // the push job whose release is not cut yet
		building = append(building, WindowRow{Kind: RowBuilding, Commit: b.Sha, At: b.Started, JobID: b.JobID, Queued: b.Queued})
	}
	byRelease := map[string][]WindowJob{}
	for _, j := range js { // newest first
		live := j.State == "queued" || j.State == "running"
		if j.ReleaseID != nil {
			if live && j.Kind == string(kindPromote) {
				deploying[*j.ReleaseID] = true
			}
			wj := WindowJob{ID: j.ID, Kind: j.Kind, State: j.State}
			if j.StartedAt != nil && j.FinishedAt != nil {
				wj.Took = j.FinishedAt.Sub(*j.StartedAt)
			}
			byRelease[*j.ReleaseID] = append(byRelease[*j.ReleaseID], wj)
		}
	}
	commitBase := o.commitBase(ctx, st.ConfigConnectorID, st.ConfigRepo)
	var waiting, failedRows, running, older []WindowRow
	for _, r := range rs {
		row := WindowRow{
			Kind: RowRelease, ID: r.ID, Number: r.Number, Commit: r.Commit, Message: r.Message,
			By: r.CreatedBy, At: r.CreatedAt, Runs: runs[r.Number], Jobs: byRelease[r.ID],
		}
		if r.Commit != "" && commitBase != "" {
			row.CommitURL = commitBase + r.Commit
		}
		for _, id := range scoped {
			if n := runsByEn[id]; n != r.Number {
				row.Reviews = append(row.Reviews, WindowReview{EnvID: id, Env: envs[id], EnvSlug: slugs[id], Rollback: r.Number < n && isRollback(byID, heads[id], r, func() bool { return promoted[id+" "+r.ID] })})
			}
		}
		switch {
		case len(failed[r.Number]) > 0:
			for _, f := range failed[r.Number] {
				fr := row
				fr.State, fr.FailEnv, fr.FailEnvID, fr.JobID, fr.Err, fr.At = StateFailed, envs[f.env], f.env, f.job, f.err, f.at
				failedRows = append(failedRows, fr)
			}
		case deploying[r.ID]:
			row.State = StateDeploying
			running = append(running, row)
		case len(waitFor[r.Number]) > 0:
			row.State, row.Newest, row.Lacks = StateWaiting, newest[r.ID], lacks[r.ID]
			for _, id := range waitFor[r.Number] {
				if h := headOf[id]; h == r.ID || h == "" { // "" = only the rung says it waits
					row.WaitEnvs = append(row.WaitEnvs, id)
				}
			}
			waiting = append(waiting, row)
		case len(row.Runs) > 0:
			row.State = StateRunning
			running = append(running, row)
		case r.Number < maxRun:
			row.State = StateSuperseded
			older = append(older, row)
		default:
			row.State = StateUndeployed
			older = append(older, row)
		}
	}
	if len(older) > windowOlder && !history {
		older = older[:windowOlder]
	}
	pf, pushFailed := o.PushFailure(ctx, stackID)
	if pushFailed {
		row := WindowRow{Kind: RowFailed, State: StateFailed, JobID: pf.JobID, Err: pf.Error, Commit: pf.Sha, By: pf.Source, At: pf.At}
		if pf.Sha != "" && commitBase != "" {
			row.CommitURL = commitBase + pf.Sha
		}
		failedRows = append(failedRows, row)
	}
	slices.SortStableFunc(failedRows, func(a, b WindowRow) int { return b.At.Compare(a.At) }) // when each failed, newest first
	w.Waiting, w.Failed = waitingCount(waiting), len(failedRows)
	var fixed []WindowRow
	if history {
		skip := map[string]bool{pf.JobID: true}
		for _, fr := range failedRows {
			skip[fr.JobID] = true
		}
		fixed = fixedFailures(js, rs, scoped, envs, skip)
	}
	for _, g := range [][]WindowRow{building, failedRows, waiting, running, older, fixed} {
		w.Rows = append(w.Rows, g...)
	}
	for i := range w.Rows {
		w.Rows[i].StackID = stackID
		if r := &w.Rows[i]; r.Kind == RowRelease && r.Message == "" && r.By == "image-watch" {
			r.Message = o.releases.MovedOf(ctx, r.ID) // "immich-server 3.4.1 to 3.4.2"
		}
	}
	return w, nil
}

// waitingCount is how many envs have something waiting: the actionable
// newest release of each counts once, however many older ones are listed.
func waitingCount(rows []WindowRow) int {
	envs := map[string]bool{}
	for _, r := range rows {
		for _, id := range r.WaitEnvs {
			envs[id] = true
		}
	}
	return len(envs)
}

// commitBase is the git host's commit URL up to the sha, "" when the stack
// names no repo or its connector no host.
func (o *Orchestrator) commitBase(ctx context.Context, connectorID, repo string) string {
	if strings.Contains(repo, "://") {
		return strings.TrimSuffix(repo, ".git") + "/commit/"
	}
	if connectorID == "" || repo == "" {
		return ""
	}
	c, err := o.store.Connectors.Get(ctx, connectorID)
	if err != nil || c.Host == "" {
		return ""
	}
	h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c.Host, "https://"), "http://"), "/")
	return "https://" + h + "/" + strings.TrimSuffix(repo, ".git") + "/commit/"
}

// IsRollback says taking releaseID into envID is a roll back: it is something
// the env ran before, or an ancestor of what it runs, and older than it. A
// newer release, or an older one the env never held (cut on another line),
// is a plain plan. A release cut before migration 019 has no lineage: the
// number decides, as it did.
func (o *Orchestrator) IsRollback(ctx context.Context, envID, releaseID string) bool {
	e, err := o.envs.Get(ctx, envID)
	if err != nil || e.ReleaseID == nil || *e.ReleaseID == releaseID {
		return false
	}
	rs, err := o.releases.List(ctx, e.StackID)
	if err != nil {
		return false
	}
	byID := make(map[string]store.Release, len(rs))
	for _, r := range rs {
		byID[r.ID] = r
	}
	t, ok := byID[releaseID]
	if !ok {
		return false
	}
	return isRollback(byID, *e.ReleaseID, t, func() bool {
		js, err := o.store.Jobs.ListTouching(ctx, []string{"env:" + envID}, string(kindPromote), 50) // newest first
		return err == nil && slices.ContainsFunc(js, func(j Job) bool { return j.State == "done" && j.ReleaseID != nil && *j.ReleaseID == releaseID })
	})
}

// isRollback is IsRollback over the stack's releases the caller already
// holds: curID is what the env runs, t the release taken into it, ranBefore
// says the env was once promoted to t.
func isRollback(byID map[string]store.Release, curID string, t store.Release, ranBefore func() bool) bool {
	cur, ok := byID[curID]
	if !ok || cur.ID == t.ID || t.Number >= cur.Number {
		return false
	}
	if t.Line == nil {
		return true
	}
	if _, ok := release.Chain(byID, cur, t.ID); ok {
		return true // an ancestor of what it runs
	}
	return ranBefore()
}

// fixedFailures are the failed pushes and promotes of a stack that a later
// done job of the same lock cured, as rows of the history. A failure no
// later job cured is the live one and is not listed again. js is newest
// first.
func fixedFailures(js []Job, rs []store.Release, scoped []string, envs map[string]string, skip map[string]bool) []WindowRow {
	lockKey := func(j Job) string {
		key := ""
		for _, l := range j.LockSet {
			if strings.HasPrefix(l, "push:") && j.Kind == string(kindPush) {
				key = l
			}
			if env, ok := strings.CutPrefix(l, "env:"); ok && j.Kind == string(kindPromote) && slices.Contains(scoped, env) {
				key = l
			}
		}
		return key
	}
	finished := func(j Job) time.Time {
		if j.FinishedAt != nil {
			return *j.FinishedAt
		}
		return j.CreatedAt
	}
	numOf := func(id string) (int, bool) {
		i := slices.IndexFunc(rs, func(r store.Release) bool { return r.ID == id })
		if i < 0 {
			return 0, false
		}
		return rs[i].Number, true
	}
	var out []WindowRow
	for i, j := range js {
		key := lockKey(j)
		if key == "" || j.State != "failed" || skip[j.ID] {
			continue
		}
		// the first done job of the same lock after it cured it (js is newest first)
		var fix *Job
		for k := i - 1; k >= 0; k-- {
			if js[k].State == "done" && lockKey(js[k]) == key && js[k].CreatedAt.After(j.CreatedAt) {
				fix = &js[k]
				break
			}
		}
		if fix == nil {
			continue
		}
		row := WindowRow{Kind: RowFailed, State: StateFailed, Fixed: true, At: finished(j), JobID: j.ID, Err: j.Error, By: "push"}
		by := -1 // cut none
		if j.Kind == string(kindPush) {
			// a push job keeps no release: the one it cut is the first push
			// release made while the fixing job ran
			from, to := fix.CreatedAt, finished(*fix)
			if fix.StartedAt != nil {
				from = *fix.StartedAt
			}
			for _, r := range slices.Backward(rs) { // oldest first
				if r.CreatedBy == "push" && !r.CreatedAt.Before(from) && !r.CreatedAt.After(to) {
					by = r.Number
					break
				}
			}
		} else {
			row.FailEnv = envs[strings.TrimPrefix(key, "env:")]
			if fix.ReleaseID != nil {
				if n, ok := numOf(*fix.ReleaseID); ok {
					by = n
				}
			}
			if j.ReleaseID != nil {
				if i := slices.IndexFunc(rs, func(r store.Release) bool { return r.ID == *j.ReleaseID }); i >= 0 {
					row.Number, row.Commit, row.Message, row.By = rs[i].Number, rs[i].Commit, rs[i].Message, rs[i].CreatedBy
				}
			}
		}
		row.FixedBy = max(by, 0)
		out = append(out, row)
	}
	return out
}
