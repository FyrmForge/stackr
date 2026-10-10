package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/imagewatch"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	Job    = store.Job
	JobLog = job.Log
)

// Job kinds: every container op is one of these, run by flow/jobs.
const (
	kindDeploy      jobs.Kind = "deploy"
	kindPromote     jobs.Kind = "promote"
	kindEnvSync     jobs.Kind = "env-sync"
	kindPush        jobs.Kind = "push"
	kindPR          jobs.Kind = "pr"
	kindDelete      jobs.Kind = "delete"
	kindRestart     jobs.Kind = "restart"
	kindStop        jobs.Kind = "stop"
	kindStart       jobs.Kind = "start"
	kindBackup      jobs.Kind = "backup"
	kindRestore     jobs.Kind = "restore"
	kindOrphans     jobs.Kind = "orphans"
	kindPanelBackup jobs.Kind = "panel-backup"
	kindUpgrade     jobs.Kind = "upgrade"
	kindImageWatch  jobs.Kind = "imagewatch"
	kindRun         jobs.Kind = "run"
	kindOrgPlan     jobs.Kind = "org-plan"
	kindOrgApply    jobs.Kind = "org-apply"
	kindServerPlan  jobs.Kind = "server-plan"
	kindServerApply jobs.Kind = "server-apply"
	kindCleanup     jobs.Kind = "cleanup"
)

type runJob struct {
	TileID string `json:"tile_id"`
	RunID  string `json:"run_id"`
}

// panelBackupJob is the panel-backup payload; Scheduled marks a cron run
// (the manual button sends none).
type panelBackupJob struct {
	Scheduled bool `json:"scheduled"`
}

type tileJob struct {
	TileID string `json:"tile_id"`
}

type promoteJob struct {
	EnvID     string `json:"env_id"`
	ReleaseID string `json:"release_id"`
	// Ticked are the secret removal rows (Change.Key) the caller approved.
	Ticked []string `json:"ticked,omitempty"`
}

type envSyncJob struct {
	EnvID  string   `json:"env_id"`
	FromID string   `json:"from_id"`
	Keep   []string `json:"keep"`
	Sig    string   `json:"sig"`
	// Owed is set once the sync parks after its writes: the tiles still to
	// deploy. A parked job runs its handler again, and a re-plan would read
	// those writes as drift.
	Owed []string `json:"owed,omitempty"`
}

type pushJob struct {
	StackID string        `json:"stack_id"`
	Event   promote.Event `json:"event"`
	// DefaultBranch is the repo's, from the push: an empty ConfigBranch
	// means it.
	DefaultBranch string `json:"default_branch,omitempty"`
}

type prJob struct {
	StackID string `json:"stack_id"`
	Action  string `json:"action"` // opened | reopened | synchronize | closed
	Number  int    `json:"number"`
	Repo    string `json:"repo"`
	Head    string `json:"head"`
	SHA     string `json:"sha"`
	Base    string `json:"base"`
	Fork    bool   `json:"fork,omitempty"` // head repo differs from the repo the PR targets
}

type backupJob struct {
	VolumeID   string `json:"volume_id"`
	ScheduleID string `json:"schedule_id,omitempty"` // "" = manual
	DestID     string `json:"dest_id,omitempty"`     // manual only; "" = local
	Method     string `json:"method,omitempty"`
	Mode       string `json:"mode,omitempty"`
}

type restoreJob struct {
	RunID          string `json:"run_id"`
	SourceVolumeID string `json:"source_volume_id"`
	TargetVolumeID string `json:"target_volume_id"`
	Method         string `json:"method,omitempty"`
}

type upgradeJob struct {
	Tag string `json:"tag"`
}

// payload decodes a job's payload into P for its handler.
func payload[P any](f func(context.Context, *jobs.Run, P) error) jobs.Handler {
	return func(ctx context.Context, r *jobs.Run) error {
		var p P
		if r.Job.Payload != "" && r.Job.Payload != "null" {
			if err := json.Unmarshal([]byte(r.Job.Payload), &p); err != nil {
				return fmt.Errorf("job payload: %w", err)
			}
		}
		return f(ctx, r, p)
	}
}

// withOwed sets owed in a job payload, keeping every other key (org_id).
func withOwed(payload string, owed []string) string {
	var m map[string]any
	if json.Unmarshal([]byte(payload), &m) != nil || m == nil {
		m = map[string]any{}
	}
	m["owed"] = owed
	b, _ := json.Marshal(m)
	return string(b)
}

func (o *Orchestrator) handlers() map[jobs.Kind]jobs.Handler {
	return map[jobs.Kind]jobs.Handler{
		kindDeploy: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			ctx = withRanFirst(ctx, r.Job.CreatedAt)
			if err := o.deploy.Redeploy(ctx, p.TileID, r.Log, r.Swap); err != nil {
				return parkOnApproval(r, err)
			}
			return o.afterDeploy(ctx, []string{p.TileID}, false)
		}),
		kindPromote: payload(func(ctx context.Context, r *jobs.Run, p promoteJob) error {
			ctx = withRanFirst(ctx, r.Job.CreatedAt)
			pre, _ := o.tiles.List(ctx, p.EnvID)
			plan, err := o.promote.ApplyTicked(ctx, p.EnvID, p.ReleaseID, p.Ticked, r.Log, r.Swap)
			if plan != nil {
				err = errors.Join(err, o.dropRuns(plan.Removed), o.dropRemoved(ctx, pre, plan.Removed), o.afterDeploy(ctx, plan.Deployed, true))
			}
			return parkOnApproval(r, err)
		}),
		kindEnvSync: payload(func(ctx context.Context, r *jobs.Run, p envSyncJob) error {
			ctx = withRanFirst(ctx, r.Job.CreatedAt)
			var plan *promote.Sync
			var err error
			pre, _ := o.tiles.List(ctx, p.EnvID)
			if len(p.Owed) > 0 {
				// Parked after its writes: deploy what it still owes, no re-plan.
				plan, err = o.promote.SyncRollout(ctx, p.Owed, r.Log, r.Swap)
			} else {
				plan, err = o.promote.SyncApply(ctx, p.EnvID, p.FromID, p.Keep, p.Sig, r.Log, r.Swap)
			}
			_, unset := errs.IsUnset(err)
			_, approval := errs.IsNeedsApproval(err)
			if (unset || approval) && plan != nil && len(plan.Owed) > 0 {
				r.Job.Payload = withOwed(r.Job.Payload, plan.Owed)
			}
			if plan != nil {
				err = errors.Join(err, o.dropRuns(plan.Removed), o.dropRemoved(ctx, pre, plan.Removed), o.afterDeploy(ctx, plan.Deployed, true))
			}
			return parkOnApproval(r, err)
		}),
		kindRun: payload(func(ctx context.Context, r *jobs.Run, p runJob) error {
			return o.run.Do(ctx, p.RunID, r.Log)
		}),
		kindPush: payload(func(ctx context.Context, r *jobs.Run, p pushJob) error {
			if err := o.ladderEnvs(ctx, p, r.Log); err != nil {
				return err
			}
			return o.runPush(ctx, p.StackID, p.Event, r.Log)
		}),
		kindPR:     payload(o.runPR),
		kindDelete: payload(o.runDelete),
		kindRestart: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			return o.onTile(ctx, p.TileID, func(t Tile) error { return o.container.Restart(ctx, t, r.Log) })
		}),
		kindStop: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			return o.onTile(ctx, p.TileID, func(t Tile) error { return o.container.Stop(ctx, t, r.Log) })
		}),
		kindStart: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			return o.onTile(ctx, p.TileID, func(t Tile) error { return o.container.Start(ctx, t, r.Log) })
		}),
		kindBackup:  payload(o.runBackup),
		kindRestore: payload(o.runRestore),
		kindOrphans: func(ctx context.Context, r *jobs.Run) error {
			days, err := o.settings.Int(ctx, "orphan_retention_days")
			if err != nil {
				return err
			}
			local, err := o.localDest(ctx)
			if err != nil {
				return err
			}
			return o.backup.Orphans(ctx, time.Duration(days)*24*time.Hour, local, r.Log)
		},
		kindPanelBackup: payload(func(ctx context.Context, r *jobs.Run, p panelBackupJob) error {
			trigger := "manual"
			if p.Scheduled {
				trigger = "schedule"
			}
			_, err := o.panelBackup(ctx, r.Log, trigger)
			return err
		}),
		kindUpgrade: payload(func(ctx context.Context, r *jobs.Run, p upgradeJob) error {
			archive, err := o.upgrade.Upgrade(ctx, p.Tag, r.Log)
			if err == nil {
				// The local destination is <data>/backups; the key is the path under it.
				_, _ = fmt.Fprintf(r.Log, "pre-upgrade archive %s; the helper swaps the panel now\n"+
					"way back if the new panel misbehaves, on the host: stackr-install restore %s\n",
					archive, filepath.Join(o.cfg.DataDir, "backups", archive))
			}
			return err
		}),
		kindImageWatch: payload(func(ctx context.Context, r *jobs.Run, sc imagewatch.Scope) error {
			ups, err := o.watch.Check(ctx, sc, r.Log)
			for _, u := range ups {
				if u.Auto {
					if _, perr := o.enqueuePromote(ctx, u.EnvID, u.ReleaseID); perr != nil {
						err = errors.Join(err, perr)
					} else {
						_, _ = fmt.Fprintf(r.Log, "%s: promoting release #%d\n", u.Env, u.Number)
					}
				}
			}
			return err
		}),
		kindOrgPlan: payload(func(ctx context.Context, r *jobs.Run, p orgPlanJob) error {
			_, err := o.planOrg(ctx, p.OrgID, r.Log)
			return err
		}),
		kindOrgApply:    payload(o.runOrgApply),
		kindServerPlan:  payload(o.runServerPlan),
		kindServerApply: payload(o.runServerApply),
		kindCleanup:     o.runCleanup,
	}
}

// enqueue adds a job row; lock is its lock set (flow/jobs supersedes an
// older job of the same kind whose set is inside the newer one's).
func (o *Orchestrator) enqueue(ctx context.Context, kind jobs.Kind, p any, lock ...string) (Job, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return Job{}, err
	}
	return o.jobs.Enqueue(ctx, kind, lock, string(o.withOrg(ctx, b)), nil)
}

// enqueuePromote locks the env and every tile it runs now.
func (o *Orchestrator) enqueuePromote(ctx context.Context, envID, releaseID string, ticked ...string) (Job, error) {
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return Job{}, err
	}
	lock := []string{"env:" + envID}
	for _, t := range ts {
		lock = append(lock, t.ID)
	}
	b, _ := json.Marshal(promoteJob{EnvID: envID, ReleaseID: releaseID, Ticked: ticked})
	return o.jobs.Enqueue(ctx, kindPromote, lock, string(o.withOrg(ctx, b)), &releaseID)
}

func (o *Orchestrator) onTile(ctx context.Context, id string, f func(Tile) error) error {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return err
	}
	return f(t)
}

// runPush turns a push into a release and promotes it into the auto envs.
func (o *Orchestrator) runPush(ctx context.Context, stackID string, ev promote.Event, log io.Writer) error {
	rel, auto, err := o.promote.Push(ctx, stackID, ev, log)
	if err != nil || rel.ID == "" {
		return err
	}
	_, _ = fmt.Fprintf(log, "release #%d\n", rel.Number)
	for _, e := range auto {
		if _, err := o.enqueuePromote(ctx, e.ID, rel.ID); err != nil {
			return err
		}
	}
	return nil
}

// ladderEnvs makes the envs the stack file's ladder names and the stack
// lacks, bottom rung first, on a push to the config repo's branch (DECIDE
// 189), then puts the ladder in the file's order. It never deletes one. A
// file that does not load is logged and the push goes on; the promote plan
// reports it.
func (o *Orchestrator) ladderEnvs(ctx context.Context, p pushJob, log io.Writer) error {
	st, err := o.stacks.Get(ctx, p.StackID)
	if err != nil {
		return err
	}
	ev := p.Event
	if st.ConfigRepo == "" || promote.NormalizeRepo(st.ConfigRepo) != promote.NormalizeRepo(ev.Repo) ||
		ev.Branch != cmp.Or(st.ConfigBranch, p.DefaultBranch) {
		return nil
	}
	org, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return err
	}
	data, fetch, err := o.stackFile(ctx, st, ev.Commit, log)
	var file *promote.Resolved
	if err == nil {
		file, err = promote.Load(data, fetch, org.Slug)
	}
	if err != nil {
		_, _ = fmt.Fprintf(log, "stack file: %v; no envs made\n", err)
		return nil
	}
	have, err := o.envs.Ladder(ctx, st.ID)
	if err != nil {
		return err
	}
	made := false
	for _, name := range file.Order {
		if slices.ContainsFunc(have, func(e store.Environment) bool { return e.Slug == name }) {
			continue
		}
		re := file.Envs[name]
		if re.FromKind == "" {
			_, _ = fmt.Fprintf(log, "env %s: the file gives it no branch or promote; not made\n", name)
			continue
		}
		if t, in, err := o.tiers.Of(ctx, st.OrgID, name); err != nil {
			return err
		} else if in && t.Locked {
			_, _ = fmt.Fprintf(log, "env %s: joins locked tier %s; an org owner makes it in the panel; not made\n", name, name)
			continue
		}
		e, err := o.envs.Create(ctx, st.ID, name, environment.Spec{
			Type:       environment.Static,
			FromKind:   re.FromKind,
			FromBranch: re.FromBranch,
			Auto:       re.Auto,
			Color:      re.Color,
		})
		if bad, ok := errs.IsInvalid(err); ok {
			_, _ = fmt.Fprintf(log, "env %s: %s; not made\n", name, bad.Msg)
			continue
		}
		if err != nil {
			return err
		}
		have = append(have, e)
		made = true
		from := strings.TrimSpace(e.FromKind + " " + e.FromBranch)
		_, _ = fmt.Fprintf(log, "env %s made from the stack file (%s)\n", e.Slug, from)
	}
	if !made {
		return nil
	}
	var ids []string
	for _, name := range file.Order {
		if i := slices.IndexFunc(have, func(e store.Environment) bool { return e.Slug == name }); i >= 0 {
			ids = append(ids, have[i].ID)
		}
	}
	if len(ids) != len(have) {
		// ponytail: a ladder with rungs the file does not name keeps the new
		// ones on top; reorder by hand, or name every rung in the file.
		_, _ = fmt.Fprintf(log, "the ladder has envs the file does not name; new envs stay on top\n")
		return nil
	}
	err = o.envs.Reorder(ctx, st.ID, ids)
	if bad, ok := errs.IsInvalid(err); ok {
		_, _ = fmt.Fprintf(log, "ladder order: %s; left as made\n", bad.Msg)
		return nil
	}
	return err
}

// runPR makes, refreshes or removes the pr-<n> env of one stack.
func (o *Orchestrator) runPR(ctx context.Context, r *jobs.Run, p prJob) error {
	name := "pr-" + strconv.Itoa(p.Number)
	e, err := o.envs.GetBySlug(ctx, p.StackID, name)
	exists := err == nil
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return err
	}
	if p.Action == "closed" {
		if !exists || e.Type != environment.Ephemeral {
			return nil
		}
		ts, err := o.tiles.List(ctx, e.ID)
		if err != nil {
			return err
		}
		if _, err := o.promote.Remove(ctx, e, ts, r.Log); err != nil {
			return err
		}
		if err := o.dropRuns(ids(ts)); err != nil {
			return err
		}
		o.sched.Reload(ctx)
		if err := o.DeleteEnv(ctx, e.ID); err != nil {
			return err
		}
		return o.sync.Sync(ctx)
	}
	on, forks, err := o.prEnabled(ctx, p.StackID, r.Log)
	if err != nil {
		_, _ = fmt.Fprintf(r.Log, "%v; PR envs off\n", err)
		return nil
	}
	if !on {
		_, _ = fmt.Fprintf(r.Log, "PR envs are off for this stack; set pr_envs.enabled in the stack file\n")
		return nil
	}
	if p.Fork && !forks {
		_, _ = fmt.Fprintf(r.Log, "pull request %d is from a fork; set pr_envs.forks to build those\n", p.Number)
		return nil
	}
	if !exists {
		envs, err := o.envs.Ladder(ctx, p.StackID)
		if err != nil {
			return err
		}
		var base *store.Environment
		for i := range envs {
			if envs[i].FromKind == environment.FromBranch && envs[i].FromBranch == p.Base {
				base = &envs[i]
				break
			}
		}
		if base == nil {
			_, _ = fmt.Fprintf(r.Log, "no env builds %s here; no PR env\n", p.Base)
			return nil
		}
		pr, err := o.envs.CloneRow(ctx, *base, name, p.Head)
		if err != nil {
			return err
		}
		if err := o.seedPREnv(ctx, pr); err != nil {
			return err
		}
	}
	return o.runPush(ctx, p.StackID, promote.Event{Repo: p.Repo, Branch: p.Head, Commit: p.SHA, PR: true, PRNumber: p.Number}, r.Log)
}

// prEnabled is pr_envs.enabled and pr_envs.forks in the stack file at the
// config branch head. A stack with no config repo is off; a file that does
// not load is an error.
func (o *Orchestrator) prEnabled(ctx context.Context, stackID string, log io.Writer) (enabled, forks bool, err error) {
	st, err := o.stacks.Get(ctx, stackID)
	if err != nil {
		return false, false, err
	}
	if st.ConfigRepo == "" {
		return false, false, nil
	}
	_, sha, err := o.configHead(ctx, st)
	if err != nil {
		return false, false, err
	}
	data, fetch, err := o.stackFile(ctx, st, sha, log)
	if err != nil {
		return false, false, fmt.Errorf("stack file at %s: %w", shortSHA(sha), err)
	}
	org, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return false, false, err
	}
	f, err := promote.Load(data, fetch, org.Slug)
	if err != nil {
		return false, false, fmt.Errorf("stack file at %s: %w", shortSHA(sha), err)
	}
	return f.PREnabled, f.PRForks, nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func (o *Orchestrator) runDelete(ctx context.Context, r *jobs.Run, p tileJob) error {
	t, err := o.tiles.Get(ctx, p.TileID)
	if err != nil {
		return err
	}
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	if _, err := o.promote.Remove(ctx, e, []store.Tile{t}, r.Log); err != nil {
		return err
	}
	if err := o.dropTileGrant(ctx, t); err != nil {
		return err
	}
	o.cancelWaitingTile(ctx, t.ID)
	if tile.RunToCompletion(t.Kind) {
		if err := o.dropRuns([]string{t.ID}); err != nil {
			return err
		}
		o.sched.Reload(ctx)
	}
	return o.sync.Sync(ctx)
}

// watchTick runs every minute: re-push the proxy config when the proxy
// container restarted (it boots from its autosave, which may be stale), and
// queue an image-watch sweep when the interval is due. It also re-declares
// the VIP rules from Docker: after a host reboot the boot rebuild can run
// before the containers are up (cost note on rebuildVIPs).
func (o *Orchestrator) watchTick(ctx context.Context) error {
	d, err := o.docker.Inspect(ctx, ProxyContainer)
	restarted := err == nil && d.Started != "" && d.Started != o.proxyStarted.Swap(d.Started)
	o.rerouteVIPs(ctx) // VIPs and filter base in one apply; a restarted proxy has a new IP in it
	if restarted {
		// A recreated proxy starts on none of the ingress networks.
		if err := o.domains.ReopenIngress(ctx); err != nil {
			slog.Warn("proxy: ingress rejoin failed", "err", err)
		}
		if err := o.sync.Sync(ctx); err != nil {
			return err
		}
	}
	due, err := o.watch.Due(ctx, time.Now())
	if err != nil || !due {
		return err
	}
	_, err = o.enqueue(ctx, kindImageWatch, imagewatch.Scope{}, "imagewatch", "imagewatch:all")
	return err
}

// GetJob returns one job row.
func (o *Orchestrator) GetJob(ctx context.Context, id string) (Job, error) {
	return o.jobs.Get(ctx, id)
}

// PollJob returns the job and its log from offset on, for a follow view.
func (o *Orchestrator) PollJob(ctx context.Context, id string, offset int64) (Job, JobLog, error) {
	return o.jobRows.Poll(ctx, id, offset)
}

// Jobs lists jobs in the given states (none = every state), newest first,
// at most limit (0 = no cap).
func (o *Orchestrator) Jobs(ctx context.Context, limit int, states ...string) ([]Job, error) {
	return o.jobRows.Recent(ctx, limit, states...)
}

// TileJobs is the newest jobs touching the tiles, newest first.
func (o *Orchestrator) TileJobs(ctx context.Context, tileIDs []string, limit int) ([]Job, error) {
	return o.jobRows.History(ctx, tileIDs, limit)
}

// CancelJob stops a queued, waiting or building job. Mid-swap is a
// Conflict, and so is an already finished job.
func (o *Orchestrator) CancelJob(ctx context.Context, id string) error {
	return o.jobs.Cancel(ctx, id)
}
