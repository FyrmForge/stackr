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

	StateWaiting    = "waiting"
	StateDeploying  = "deploying"
	StateRunning    = "running"
	StateFailed     = "failed"
	StateSuperseded = "superseded"
	StateUndeployed = "not deployed"
)

// WindowReview is one env's way into a release's plan.
type WindowReview struct {
	EnvID, Env string
	Rollback   bool
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
	Number  int    // 0 = none (a failed push, a building one)
	Commit  string
	Message string
	By      string // Release.CreatedBy
	At      time.Time
	State   string
	Runs    []string // env ids that run it
	FailEnv string   // StateFailed from a promote: the env it stopped in
	Newest  bool
	// Lacks is the number of a waiting release this one does not hold; 0 = none.
	Lacks   int
	Reviews []WindowReview
	JobID   string // the job log of a failure or a build
	Jobs    []WindowJob
	Err     string // the error of a failed push
	Queued  bool   // building: not started yet
	// CommitURL is the commit on the git host ("" = unknown).
	CommitURL string
}

// ReleaseWindow is the rows and counts of the releases window of a stack
// (envID "") or one of its envs. Rows are what needs a person first (a
// build, failures, waiting), then what each env runs, then a few older ones.
type ReleaseWindow struct {
	Rows            []WindowRow
	Waiting, Failed int
	Stack           string // the stack's name
}

// windowOlder is how many releases beyond what waits, fails or runs the
// window lists.
const windowOlder = 3

// ReleaseWindow reads the rows from releases, the ladder, the stack's jobs
// and the failure markers the canvas already shows.
func (o *Orchestrator) ReleaseWindow(ctx context.Context, stackID, envID string) (ReleaseWindow, error) {
	var w ReleaseWindow
	var envs = map[string]string{} // env id -> name
	st, err := o.stacks.Get(ctx, stackID)
	if err != nil {
		return w, err
	}
	w.Stack = st.Name
	rs, err := o.releases.List(ctx, stackID)
	if err != nil {
		return w, err
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
		envs[r.EnvID] = r.Name
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
	for _, e := range ladder {
		if envID != "" && e.ID != envID {
			continue
		}
		runsID := ""
		if e.ReleaseID != nil {
			runsID = *e.ReleaseID
		}
		lines := release.Lines(e)
		head, err := o.releases.Newest(ctx, stackID, runsID, lines...)
		if err != nil {
			return w, err
		}
		if head == runsID {
			continue
		}
		newest[head] = true
		for _, r := range rs {
			if r.Line == nil || !slices.Contains(lines, *r.Line) || r.ID == runsID || r.Number <= runsByEn[e.ID] {
				continue
			}
			path, ok, err := o.releases.Between(ctx, stackID, r.ID, runsID)
			if err != nil {
				return w, err
			}
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
	// failed promotes, by release number
	failed := map[int]string{}
	failedJob := map[int]string{}
	for _, id := range scoped {
		if s, ok := o.RolloutStop(ctx, id); ok && s.Release > 0 {
			failed[s.Release], failedJob[s.Release] = id, s.JobID
		}
	}
	// ponytail: scans the stack's job rows (ScopeJobs does); a SQL filter on
	// release_id when an install holds enough jobs to feel it.
	js, err := o.ScopeJobs(ctx, st.OrgID, stackID, "", 0)
	if err != nil {
		return w, err
	}
	var building []WindowRow
	deploying := map[string]bool{}
	if b, ok := o.Building(ctx, stackID); ok { // the push job whose release is not cut yet
		building = append(building, WindowRow{Kind: RowBuilding, Commit: b.Sha, At: b.Started, JobID: b.JobID, Queued: b.Queued})
	}
	byRelease := map[string][]WindowJob{}
	for _, j := range js { // newest first
		live := j.State == "queued" || j.State == "running"
		switch {
		case j.ReleaseID != nil:
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
				row.Reviews = append(row.Reviews, WindowReview{EnvID: id, Env: envs[id], Rollback: r.Number < n && o.IsRollback(ctx, id, r.ID)})
			}
		}
		switch {
		case failed[r.Number] != "":
			row.State, row.FailEnv, row.JobID = StateFailed, envs[failed[r.Number]], failedJob[r.Number]
			failedRows = append(failedRows, row)
		case deploying[r.ID]:
			row.State = StateDeploying
			running = append(running, row)
		case len(waitFor[r.Number]) > 0:
			row.State, row.Newest, row.Lacks = StateWaiting, newest[r.ID], lacks[r.ID]
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
	if len(older) > windowOlder {
		older = older[:windowOlder]
	}
	if pf, ok := o.PushFailure(ctx, stackID); ok {
		failedRows = append([]WindowRow{{Kind: RowFailed, State: StateFailed, JobID: pf.JobID, Err: pf.Error}}, failedRows...)
	}
	w.Waiting, w.Failed = len(waiting), len(failedRows)
	for _, g := range [][]WindowRow{building, failedRows, waiting, running, older} {
		w.Rows = append(w.Rows, g...)
	}
	return w, nil
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
	t, err := o.releases.Get(ctx, releaseID)
	if err != nil {
		return false
	}
	cur, err := o.releases.Get(ctx, *e.ReleaseID)
	if err != nil || t.Number >= cur.Number {
		return false
	}
	if t.Line == nil {
		return true
	}
	if _, ok, err := o.releases.Between(ctx, e.StackID, cur.ID, t.ID); err == nil && ok {
		return true // an ancestor of what it runs
	}
	js, err := o.store.Jobs.ListTouching(ctx, []string{"env:" + envID}, string(kindPromote), 50) // newest first
	if err != nil {
		return false
	}
	return slices.ContainsFunc(js, func(j Job) bool { return j.State == "done" && j.ReleaseID != nil && *j.ReleaseID == releaseID })
}
