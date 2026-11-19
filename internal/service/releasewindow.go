package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"
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
	var building, deploying = []WindowRow{}, map[string]bool{}
	byRelease := map[string][]WindowJob{}
	for _, j := range js { // newest first
		live := j.State == "queued" || j.State == "running"
		switch {
		case j.Kind == string(kindPush) && live:
			var p pushJob
			_ = json.Unmarshal([]byte(j.Payload), &p)
			row := WindowRow{Kind: RowBuilding, Commit: p.Event.Commit, Message: p.Event.Message, At: j.CreatedAt, JobID: j.ID, Queued: j.State == "queued"}
			if j.StartedAt != nil {
				row.At = *j.StartedAt
			}
			building = append(building, row)
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
				row.Reviews = append(row.Reviews, WindowReview{EnvID: id, Env: envs[id], Rollback: r.Number < n})
			}
		}
		switch {
		case failed[r.Number] != "":
			row.State, row.FailEnv, row.JobID = StateFailed, envs[failed[r.Number]], failedJob[r.Number]
			failedRows = append(failedRows, row)
		case len(waitFor[r.Number]) > 0:
			row.State = StateWaiting
			waiting = append(waiting, row)
		case deploying[r.ID]:
			row.State = StateDeploying
			running = append(running, row)
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
