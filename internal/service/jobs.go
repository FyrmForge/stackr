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
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/imagewatch"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/startguard"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	Job    = store.Job
	JobLog = job.Log
)

// Job kinds: every container op is one of these, run by flow/jobs.
const (
	kindDeploy         jobs.Kind = "deploy"
	kindPromote        jobs.Kind = "promote"
	kindEnvSync        jobs.Kind = "env-sync"
	kindPush           jobs.Kind = "push"
	kindPR             jobs.Kind = "pr"
	kindDelete         jobs.Kind = "delete"
	kindRestart        jobs.Kind = "restart"
	kindStop           jobs.Kind = "stop"
	kindStart          jobs.Kind = "start"
	kindBackup         jobs.Kind = "backup"
	kindRestore        jobs.Kind = "restore"
	kindOrphans        jobs.Kind = "orphans"
	kindPanelBackup    jobs.Kind = "panel-backup"
	kindUpgrade        jobs.Kind = "upgrade"
	kindImageWatch     jobs.Kind = "imagewatch"
	kindRun            jobs.Kind = "run"
	kindOrgPlan        jobs.Kind = "org-plan"
	kindOrgApply       jobs.Kind = "org-apply"
	kindServerPlan     jobs.Kind = "server-plan"
	kindServerApply    jobs.Kind = "server-apply"
	kindCleanup        jobs.Kind = "cleanup"
	kindRegistryClean  jobs.Kind = "registry-clean"
	kindMoveEnv        jobs.Kind = "move-env"
	kindShareReconcile jobs.Kind = "share-reconcile"
	kindSeed           jobs.Kind = "seed"
	kindDiskAdopt      jobs.Kind = "disk-adopt"
	kindDiskForget     jobs.Kind = "disk-forget"
	kindVolumeToDisk   jobs.Kind = "volume-to-disk" // a single move, and each group of Move all
)

type runJob struct {
	TileID string `json:"tile_id"`
	RunID  string `json:"run_id"`
}

// panelBackupJob is the panel-backup payload; Scheduled marks a cron run,
// DestID is where a manual one goes ("" local).
type panelBackupJob struct {
	Scheduled bool   `json:"scheduled"`
	DestID    string `json:"dest_id,omitempty"`
}

type tileJob struct {
	TileID string `json:"tile_id"`
	// Note opens a deploy's job log (a restore's warnings).
	Note string `json:"note,omitempty"`
	// Rebuild: build the pinned commit again without the build cache first.
	Rebuild bool `json:"rebuild,omitempty"`
}

type promoteJob struct {
	EnvID     string `json:"env_id"`
	ReleaseID string `json:"release_id"`
	// Ticked are the secret removal rows (Change.Key) the caller approved.
	Ticked []string `json:"ticked,omitempty"`
	// MayPlace: an org owner approved it, so it may change pin, ingress
	// and exits. A push's or an image watch's own promote never may.
	MayPlace bool `json:"may_place,omitempty"`
	// Admin: a server admin approved it, so standbys may use any node.
	Admin bool `json:"admin,omitempty"`
	// CleanOnly: queued from a list with no plan read, so the job refuses
	// unless its final plan has no problems (PlanProblems).
	CleanOnly bool `json:"clean_only,omitempty"`
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

// withStopped records where a failed promote stopped, keeping every other
// key: RolloutStop reads it for the canvas.
func withStopped(payload string, st *promote.Stopped) string {
	var m map[string]any
	if json.Unmarshal([]byte(payload), &m) != nil || m == nil {
		m = map[string]any{}
	}
	m["stopped_at"], m["not_deployed"] = st.Tile, st.Pending
	b, _ := json.Marshal(m)
	return string(b)
}

// withSkipped records the envs a push did not make and why, keeping every
// other key: PushState reads it for a stack with no environments.
func withSkipped(payload string, skipped []string) string {
	if len(skipped) == 0 {
		return payload
	}
	var m map[string]any
	if json.Unmarshal([]byte(payload), &m) != nil || m == nil {
		m = map[string]any{}
	}
	m["skipped"] = skipped
	b, _ := json.Marshal(m)
	return string(b)
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

// handlers names the node each job kind runs on:
//
//   - deploy, promote, env-sync, run, restart, stop, start and delete work
//     on one env, so on its node (the tile and volume leaves find it again
//     for their own Docker calls);
//   - push, image-watch, the org and server plans and applies, orphans and
//     the panel's own backup and upgrade are controller work: a push builds
//     on the controller, the plans read git and call the verbs that find
//     each env's node themselves;
//   - cleanup loops over the nodes.
func (o *Orchestrator) handlers() map[jobs.Kind]jobs.Handler {
	return map[jobs.Kind]jobs.Handler{
		kindDeploy: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			for l := range strings.SplitSeq(p.Note, "\n") {
				if l != "" {
					_, _ = fmt.Fprintln(r.Log, "warning: "+l)
				}
			}
			ctx, err := o.withTileNode(ctx, p.TileID)
			if err != nil {
				return err
			}
			ctx = withRanFirst(ctx, r.Job.CreatedAt)
			if err := o.runGateTile(ctx, p.TileID); err != nil {
				return err
			}
			o.ensureMesh(ctx)
			if p.Rebuild {
				if err := o.deploy.Rebuild(ctx, p.TileID, r.Log); err != nil {
					return err
				}
				// Built and pinned: a parked job's next run only deploys.
				// Other keys (org_id) are kept.
				var m map[string]any
				if json.Unmarshal([]byte(r.Job.Payload), &m) == nil && m != nil {
					delete(m, "rebuild")
					b, _ := json.Marshal(m)
					r.Job.Payload = string(b)
				}
			}
			ctx = startguard.With(ctx, func() error { return o.runGateTile(ctx, p.TileID) })
			swap := gatedSwap(r.Swap, func() error { return o.runGateTile(ctx, p.TileID) })
			if err := o.deploy.Redeploy(ctx, p.TileID, r.Log, swap); err != nil {
				if ph := deploy.PhaseOf(err); ph == deploy.PhasePooler || ph == deploy.PhaseReconcile {
					// the instance runs; only a step after its start failed
					err = errors.Join(err, o.afterDeploy(ctx, []string{p.TileID}, false))
				} else {
					o.downAfterFailedDeploy(ctx, p.TileID, err)
				}
				return o.parkOnApproval(ctx, r, err)
			}
			return o.afterDeploy(ctx, []string{p.TileID}, false)
		}),
		kindPromote: payload(func(ctx context.Context, r *jobs.Run, p promoteJob) error {
			ctx, err := o.withEnvIDNode(ctx, p.EnvID)
			if err != nil {
				return err
			}
			ctx = withRanFirst(ctx, r.Job.CreatedAt)
			if err := o.runGateEnv(ctx, p.EnvID); err != nil {
				return err
			}
			ctx = startguard.With(ctx, func() error { return o.runGateEnv(ctx, p.EnvID) })
			o.ensureMesh(ctx)
			pre, _ := o.tiles.List(ctx, p.EnvID)
			var guard func(*promote.Plan) error
			if p.CleanOnly {
				guard = o.cleanGuard(ctx, p.EnvID, p.ReleaseID)
			}
			plan, err := o.promote.ApplyGuarded(context.WithValue(withMayPlace(ctx, p.MayPlace), placeAdminKey{}, p.Admin), p.EnvID, p.ReleaseID, p.Ticked, guard, lockLog{ctx, o, r, p.EnvID}, gatedSwap(r.Swap, func() error { return o.runGateEnv(ctx, p.EnvID) }))
			if plan != nil {
				err = errors.Join(err, o.dropRuns(plan.Removed), o.dropRemoved(ctx, pre, plan.Removed), o.afterDeploy(ctx, plan.Deployed, true))
			}
			var st *promote.Stopped
			if errors.As(err, &st) {
				r.Job.Payload = withStopped(r.Job.Payload, st)
			}
			return o.parkOnApproval(ctx, r, err)
		}),
		kindEnvSync: payload(func(ctx context.Context, r *jobs.Run, p envSyncJob) error {
			ctx, err := o.withEnvIDNode(ctx, p.EnvID)
			if err != nil {
				return err
			}
			ctx = withRanFirst(ctx, r.Job.CreatedAt)
			if err := o.runGateEnv(ctx, p.EnvID); err != nil {
				return err
			}
			ctx = startguard.With(ctx, func() error { return o.runGateEnv(ctx, p.EnvID) })
			var plan *promote.Sync
			pre, _ := o.tiles.List(ctx, p.EnvID)
			if len(p.Owed) > 0 {
				// Parked after its writes: deploy what it still owes, no re-plan.
				plan, err = o.promote.SyncRollout(ctx, p.Owed, lockLog{ctx, o, r, p.EnvID}, gatedSwap(r.Swap, func() error { return o.runGateEnv(ctx, p.EnvID) }))
			} else {
				plan, err = o.promote.SyncApply(ctx, p.EnvID, p.FromID, p.Keep, p.Sig, lockLog{ctx, o, r, p.EnvID}, gatedSwap(r.Swap, func() error { return o.runGateEnv(ctx, p.EnvID) }))
			}
			_, unset := errs.IsUnset(err)
			_, approval := errs.IsNeedsApproval(err)
			if (unset || approval) && plan != nil && len(plan.Owed) > 0 {
				r.Job.Payload = withOwed(r.Job.Payload, plan.Owed)
			}
			if plan != nil {
				err = errors.Join(err, o.dropRuns(plan.Removed), o.dropRemoved(ctx, pre, plan.Removed), o.afterDeploy(ctx, plan.Deployed, true))
			}
			return o.parkOnApproval(ctx, r, err)
		}),
		kindRun: payload(func(ctx context.Context, r *jobs.Run, p runJob) error {
			ctx, err := o.withTileNode(ctx, p.TileID)
			if err != nil {
				return err
			}
			if err := o.runGateTile(ctx, p.TileID); err != nil {
				return err
			}
			ctx = startguard.With(ctx, func() error { return o.runGateTile(ctx, p.TileID) })
			if err := o.skipDownCron(ctx, p.RunID); err != nil {
				return err
			}
			return o.run.Do(ctx, p.RunID, r.Log)
		}),
		kindPush: payload(func(ctx context.Context, r *jobs.Run, p pushJob) error {
			ctx, err := o.withController(ctx)
			if err != nil {
				return err
			}
			skipped, err := o.ladderEnvs(ctx, p, r.Log)
			if err != nil {
				return err
			}
			// the reasons outlive the log: the empty stack page says them
			r.Job.Payload = withSkipped(r.Job.Payload, skipped)
			p.Event.Swap = r.Swap
			return o.runPush(ctx, p.StackID, p.Event, r.Log)
		}),
		kindPR:            payload(o.runPR),
		kindDelete:        payload(o.runDelete),
		kindStackTeardown: payload(o.runStackTeardown),
		kindRestart: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			return o.onTile(ctx, p.TileID, func(ctx context.Context, t Tile) error {
				if err := o.mayStartTile(ctx, t); err != nil {
					return err
				}
				if err := o.runGateTile(ctx, t.ID); err != nil {
					return err
				}
				return o.container.Restart(ctx, t, r.Log)
			})
		}),
		kindStop: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			return o.onTile(ctx, p.TileID, func(ctx context.Context, t Tile) error {
				if err := o.container.Stop(ctx, t, r.Log); err != nil {
					return err
				}
				o.clearProblem(ctx, downAlertKey(t.ID)) // stopped on purpose is not down
				o.setPrimaryState(ctx, t.ID, opmember.Stopped)
				return nil
			})
		}),
		kindStart: payload(func(ctx context.Context, r *jobs.Run, p tileJob) error {
			return o.onTile(ctx, p.TileID, func(ctx context.Context, t Tile) error {
				if err := o.mayStartTile(ctx, t); err != nil {
					return err
				}
				if err := o.runGateTile(ctx, t.ID); err != nil {
					return err
				}
				if err := o.container.Start(ctx, t, r.Log); err != nil {
					return err
				}
				o.setPrimaryState(ctx, t.ID, opmember.Running)
				return nil
			})
		}),
		kindBackup: payload(func(ctx context.Context, r *jobs.Run, p backupJob) error {
			err := o.runBackup(ctx, r, p)
			if p.ScheduleID != "" {
				subject, nodeID := "", ""
				if err != nil { // a success only resets: no lookups
					subject, nodeID = "volume "+p.VolumeID, o.volumeNode(ctx, p.VolumeID)
					if v, verr := o.store.Volumes.Get(ctx, p.VolumeID); verr == nil {
						subject = "volume " + v.Slug
					}
				}
				o.backupResult(ctx, "schedule."+p.ScheduleID, nodeID, subject, err)
			}
			return err
		}),
		kindRestore:       payload(o.runRestore),
		kindSeed:          payload(o.runSeed),
		kindBucketRestore: payload(o.runBucketRestore),
		kindBucketSync:    payload(o.runBucketSync),

		kindRestoreInstance: payload(o.runRestoreInstance),
		kindPromoteRestore:  payload(o.runPromoteRestore),
		kindPGUpgrade:       payload(o.runPGUpgrade),
		kindFailover:        payload(o.runFailoverJob),
		kindPGRollback:      payload(o.runPGRollback),
		kindPGExt:           payload(o.runPGExt),
		kindOrphans: func(ctx context.Context, r *jobs.Run) error {
			ctx, err := o.withController(ctx)
			if err != nil {
				return err
			}
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
			ctx, err := o.withController(ctx)
			if err != nil {
				return err
			}
			trigger := "manual"
			if p.Scheduled {
				trigger = "schedule"
			}
			_, err = o.panelBackup(ctx, r.Log, trigger, p.DestID)
			// either kind opens the problem on a failure and clears it on a success
			o.backupResult(ctx, "panel", "", "the panel database", err)
			return err
		}),
		kindUpgrade: payload(func(ctx context.Context, r *jobs.Run, p upgradeJob) error {
			ctx, err := o.withController(ctx)
			if err != nil {
				return err
			}
			archive, err := o.upgrade.Upgrade(ctx, p.Tag, r.Log)
			if err != nil {
				o.upgradeStep.Store(0)
			} else {
				o.holdUpgrade() // the helper runs: hold until it exits
				// The local destination is <data>/backups; the key is the path under it.
				_, _ = fmt.Fprintf(r.Log, "pre-upgrade archive %s; the helper swaps the panel now\n"+
					"way back if the new panel misbehaves, on the host: stackr-install restore %s\n",
					archive, filepath.Join(o.cfg.DataDir, "backups", archive))
			}
			return err
		}),
		kindMovePanelDB: payload(o.runMovePanelDB),
		kindImageWatch: payload(func(ctx context.Context, r *jobs.Run, sc imagewatch.Scope) error {
			ctx, err := o.withController(ctx)
			if err != nil {
				return err
			}
			ups, err := o.watch.Check(ctx, sc, r.Log)
			for _, u := range ups {
				if u.Auto {
					if _, perr := o.enqueuePromote(ctx, u.EnvID, u.ReleaseID, false, false); perr != nil {
						err = errors.Join(err, perr)
					} else {
						_, _ = fmt.Fprintf(r.Log, "%s: promoting release #%d\n", u.Env, u.Number)
					}
				}
			}
			return err
		}),
		kindOrgPlan: payload(func(ctx context.Context, r *jobs.Run, p orgPlanJob) error {
			ctx, err := o.withController(ctx)
			if err != nil {
				return err
			}
			_, err = o.planOrg(ctx, p.OrgID, r.Log)
			return err
		}),
		kindOrgApply:       controllerJob(o, payload(o.runOrgApply)),
		kindServerPlan:     controllerJob(o, payload(o.runServerPlan)),
		kindServerApply:    controllerJob(o, payload(o.runServerApply)),
		kindCleanup:        o.runCleanup,
		kindShareReconcile: o.runShareReconcile,
		kindRegistryClean:  controllerJob(o, payload(o.runRegistryClean)),
		kindMoveEnv:        payload(o.runMove),
		kindRemoveNode:     controllerJob(o, payload(o.runRemoveNode)),
		kindForgetNode:     controllerJob(o, payload(o.runForgetNode)),
		kindNodeBackup:     controllerJob(o, payload(o.runNodeBackup)),
		kindDiskAdd:        controllerJob(o, payload(o.runDiskAdd)),
		kindDiskCheck:      controllerJob(o, payload(o.runDiskCheck)),
		kindDiskQuarantine: controllerJob(o, payload(o.runDiskQuarantine)),
		kindDiskAddDevice:  controllerJob(o, payload(o.runDiskAddDevice)),
		kindDiskGrow:       controllerJob(o, payload(o.runDiskGrow)),
		kindDiskAdopt:      controllerJob(o, ownedJob(payload(o.runDiskAdopt))),
		kindDiskForget:     controllerJob(o, ownedJob(payload(o.runDiskForget))),
		kindVolumeToDisk:   controllerJob(o, ownedJob(payload(o.runVolumeToDisk))),
		kindWorkerRollout:  controllerJob(o, o.runWorkerRollout),
	}
}

// controllerJob runs h on the controller: its Docker calls are the panel's
// own, or go through leaves that find an env's node themselves.
func controllerJob(o *Orchestrator, h jobs.Handler) jobs.Handler {
	return func(ctx context.Context, r *jobs.Run) error {
		ctx, err := o.withController(ctx)
		if err != nil {
			return err
		}
		return h(ctx, r)
	}
}

// enqueue adds a job row; lock is its lock set (flow/jobs supersedes an
// older job of the same kind whose set is inside the newer one's).
func (o *Orchestrator) enqueue(ctx context.Context, kind jobs.Kind, p any, lock ...string) (Job, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return Job{}, err
	}
	j, err := o.jobs.Enqueue(ctx, kind, lock, string(o.withOrg(ctx, b)), nil)
	return j, o.noted(ctx, err, securitylog.JobQueue, "job", j.ID, "kind="+string(kind))
}

// enqueueExclusive is enqueue that refuses with busy, atomically, when a live
// job of the kind holds any of keys (a second press never cancels the first).
func (o *Orchestrator) enqueueExclusive(ctx context.Context, kind jobs.Kind, p any, keys []string, busy error, lock ...string) (Job, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return Job{}, err
	}
	j, err := o.jobs.EnqueueExclusive(ctx, kind, lock, string(o.withOrg(ctx, b)), nil, keys, busy)
	if err != nil && j.ID == "" {
		return j, err
	}
	return j, o.noted(ctx, err, securitylog.JobQueue, "job", j.ID, "kind="+string(kind))
}

// enqueuePromote locks the env and every tile it runs now.
func (o *Orchestrator) enqueuePromote(ctx context.Context, envID, releaseID string, mayPlace, admin bool, ticked ...string) (Job, error) {
	return o.enqueuePromoteJob(ctx, promoteJob{EnvID: envID, ReleaseID: releaseID, Ticked: ticked, MayPlace: mayPlace, Admin: admin})
}

func (o *Orchestrator) enqueuePromoteJob(ctx context.Context, pj promoteJob) (Job, error) {
	envID, releaseID := pj.EnvID, pj.ReleaseID
	if err := o.gateEnv(ctx, envID, true); err != nil {
		return Job{}, err
	}
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return Job{}, err
	}
	lock := []string{"env:" + envID}
	for _, t := range ts {
		lock = append(lock, t.ID)
	}
	b, _ := json.Marshal(pj)
	j, err := o.jobs.Enqueue(ctx, kindPromote, lock, string(o.withOrg(ctx, b)), &releaseID)
	return j, o.noted(ctx, err, securitylog.JobQueue, "job", j.ID, "kind=promote release="+releaseID)
}

// onTile runs f on the tile's node.
func (o *Orchestrator) onTile(ctx context.Context, id string, f func(context.Context, Tile) error) error {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return err
	}
	ctx, err = o.withEnvIDNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	return f(ctx, t)
}

// runPush turns a push into a release and promotes it into the auto envs.
func (o *Orchestrator) runPush(ctx context.Context, stackID string, ev promote.Event, log io.Writer) error {
	rel, auto, err := o.promote.Push(ctx, stackID, ev, log)
	if err != nil || rel.ID == "" {
		return err
	}
	_, _ = fmt.Fprintf(log, "release #%d\n", rel.Number)
	for _, e := range auto {
		if _, err := o.enqueuePromote(ctx, e.ID, rel.ID, false, false); err != nil {
			return err
		}
	}
	return nil
}

// ladderEnvs makes the envs the stack file's ladder names and the stack
// lacks, bottom rung first, on a push to the config repo's branch (DECIDE
// 189), then puts the ladder in the file's order. It never deletes one. A
// file that does not load is logged and the push goes on; the promote plan
// reports it. skipped is why each env the file names was not made.
func (o *Orchestrator) ladderEnvs(ctx context.Context, p pushJob, log io.Writer) (skipped []string, err error) {
	st, err := o.stacks.Get(ctx, p.StackID)
	if err != nil {
		return nil, err
	}
	ev := p.Event
	if st.ConfigRepo == "" || promote.NormalizeRepo(st.ConfigRepo) != promote.NormalizeRepo(ev.Repo) ||
		ev.Branch != cmp.Or(st.ConfigBranch, p.DefaultBranch) {
		return nil, nil
	}
	org, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return nil, err
	}
	data, fetch, err := o.stackFile(ctx, st, ev.Commit, log)
	var file *promote.Resolved
	if err == nil {
		file, err = promote.Load(data, fetch, org.Slug)
	}
	if err != nil {
		_, _ = fmt.Fprintf(log, "stack file: %v; no envs made\n", err)
		return []string{"the stack file does not load: " + strings.SplitN(err.Error(), "\n", 2)[0]}, nil
	}
	have, err := o.envs.Ladder(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	made := false
	for _, name := range file.Order {
		if slices.ContainsFunc(have, func(e store.Environment) bool { return e.Slug == name }) {
			continue
		}
		re := file.Envs[name]
		if re.FromKind == "" {
			_, _ = fmt.Fprintf(log, "env %s: the file gives it no branch or promote; not made\n", name)
			skipped = append(skipped, name+": the file gives it no branch or promote")
			continue
		}
		if t, in, err := o.tiers.Of(ctx, st.OrgID, name); err != nil {
			return nil, err
		} else if in && t.Locked {
			_, _ = fmt.Fprintf(log, "env %s: joins locked tier %s; an org owner makes it in the panel; not made\n", name, name)
			skipped = append(skipped, name+": it joins a locked tier, an org owner makes it in the panel")
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
			skipped = append(skipped, name+": "+bad.Msg)
			continue
		}
		if err != nil {
			return nil, err
		}
		have = append(have, e)
		made = true
		from := strings.TrimSpace(e.FromKind + " " + e.FromBranch)
		_, _ = fmt.Fprintf(log, "env %s made from the stack file (%s)\n", e.Slug, from)
	}
	if !made {
		return skipped, nil
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
		return skipped, nil
	}
	err = o.envs.Reorder(ctx, st.ID, ids)
	if bad, ok := errs.IsInvalid(err); ok {
		_, _ = fmt.Fprintf(log, "ladder order: %s; left as made\n", bad.Msg)
		return skipped, nil
	}
	return skipped, err
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
		if ctx, err = o.withEnvNode(ctx, e); err != nil {
			return err
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
		prCtx, err := o.withEnvNode(ctx, pr)
		if err != nil {
			return err
		}
		if err := o.seedPREnv(prCtx, pr); err != nil {
			return err
		}
	}
	// A PR env builds its head; a row an older plan moved off it is set back.
	if exists && e.Type == environment.Ephemeral &&
		(e.FromKind != environment.FromBranch || e.FromBranch != p.Head || !e.Auto) {
		if _, err := o.envs.SetFrom(ctx, e, environment.FromBranch, p.Head, true); err != nil {
			return err
		}
	}
	// The build runs on the controller; the promotes it queues run on each env's node.
	if ctx, err = o.withController(ctx); err != nil {
		return err
	}
	return o.runPush(ctx, p.StackID, promote.Event{Repo: p.Repo, Branch: p.Head, Commit: p.SHA, PR: true, PRNumber: p.Number, Swap: r.Swap}, r.Log)
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
	if ctx, err = o.withEnvNode(tearingDown(ctx), e); err != nil {
		return err
	}
	rel, away := o.relocated(ctx, t.ID) // read before the instance row goes
	if _, err := o.promote.Remove(ctx, e, []store.Tile{t}, r.Log); err != nil {
		return err
	}
	if away {
		if err := o.dropRelocated(ctx, t, rel); err != nil {
			return err
		}
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

// watchTick runs every minute, in three scopes:
//
//   - the controller: re-push the proxy config when the proxy container
//     restarted (it boots from its autosave, which may be stale);
//   - every node: re-declare the VIP rules from Docker (after a host reboot
//     the boot rebuild can run before the containers are up; cost note on
//     rebuildVIPs). A restarted proxy has a new IP in the filter base, so
//     this runs before the proxy's networks are rejoined;
//   - every env, as one job: queue an image-watch sweep when the interval
//     is due.
func (o *Orchestrator) watchTick(ctx context.Context) error {
	restarted, err := o.proxyRestarted(ctx)
	if err != nil {
		return err
	}
	o.finishRemovals(ctx)
	o.resumeFailovers(ctx)    // a failover the panel left half done, an old primary whose node is back
	o.startMembers(ctx)       // before the rebuild: it routes to what runs
	o.syncStandbyPoolers(ctx) // before the probes: a standby joins the read pool with its pooler up
	o.observeStandbys(ctx)
	o.observePrimaries(ctx)
	o.watchStandbys(ctx)
	o.rerouteVIPs(ctx)
	o.recheckFailingCerts(ctx)
	if restarted {
		if err := o.rejoinProxy(ctx); err != nil {
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

// trafficTick is the scheduler's traffic sample: the conntrack table it
// reads is the controller host's, so it is controller work.
func (o *Orchestrator) trafficTick(ctx context.Context) error {
	ctx, err := o.withController(ctx)
	if err != nil {
		return err
	}
	return o.sample.Tick(ctx)
}

// proxyRestarted reports whether the proxy container (on the controller) has
// a new start time since the last tick.
func (o *Orchestrator) proxyRestarted(ctx context.Context) (bool, error) {
	ctx, err := o.withController(ctx)
	if err != nil {
		return false, err
	}
	d, err := o.docker.Inspect(ctx, ProxyContainer)
	return err == nil && d.Started != "" && d.Started != o.proxyStarted.Swap(d.Started), nil
}

// rejoinProxy puts a recreated proxy back on the ingress networks (it starts
// on none of them) and pushes its config.
func (o *Orchestrator) rejoinProxy(ctx context.Context) error {
	ctx, err := o.withController(ctx)
	if err != nil {
		return err
	}
	if err := o.domains.ReopenIngress(ctx); err != nil {
		slog.Warn("proxy: ingress rejoin failed", "err", err)
	}
	return o.sync.Sync(ctx)
}

// GetJob returns one job row, with what the jobs lists say about it.
func (o *Orchestrator) GetJob(ctx context.Context, id string) (Job, error) {
	j, err := o.jobs.Get(ctx, id)
	if err != nil {
		return j, err
	}
	js, err := o.annotate(ctx, []Job{j})
	return js[0], err
}

// annotate fills the jobs lists' Line and Waits, which are never stored.
func (o *Orchestrator) annotate(ctx context.Context, js []Job) ([]Job, error) {
	var live []Job
	if slices.ContainsFunc(js, func(j Job) bool { return j.State == job.Queued }) {
		var err error
		if live, err = o.jobRows.List(ctx, job.Queued, job.Running); err != nil {
			return js, err
		}
	}
	held := o.UpgradeHeld()
	for i := range js {
		js[i].Scope = o.jobScope(ctx, js[i])
		js[i].Waits = job.Waits(js[i], live, held)
		if js[i].Waits == "" {
			js[i].Waits = o.parkedWaits(js[i])
		}
		js[i].Line = jobLine(js[i])
		if js[i].Kind == string(kindPromote) {
			var p promoteJob
			if json.Unmarshal([]byte(js[i].Payload), &p) == nil {
				if r, err := o.releases.Get(ctx, p.ReleaseID); err == nil {
					js[i].Line = fmt.Sprintf("promote of release %d", r.Number)
				}
			}
		}
	}
	return js, nil
}

// parkedWaits is what a job parked on a value (or an approval) waits for, ""
// for a job that is not waiting: a resumed one is queued or running again and
// waits for nothing.
func (o *Orchestrator) parkedWaits(j Job) string {
	if j.State != job.Waiting || j.WaitingParam == nil {
		return ""
	}
	return "Waits for " + *j.WaitingParam
}

// jobScope names the stack, or stack/env, a job's payload is about, "" for a
// job above the stacks (or whose rows are gone).
// ponytail: two row reads per job listed; cache by id if lists grow large.
func (o *Orchestrator) jobScope(ctx context.Context, j Job) string {
	var p map[string]any
	_ = json.Unmarshal([]byte(j.Payload), &p)
	c := o.jobChild(ctx, p)
	if c.Stack == "" {
		return ""
	}
	st, err := o.stacks.Get(ctx, c.Stack)
	if err != nil {
		return ""
	}
	if c.Env == "" {
		return st.Slug
	}
	if e, err := o.envs.Get(ctx, c.Env); err == nil {
		return st.Slug + "/" + e.Slug
	}
	return st.Slug
}

// jobLine is the release line a job makes, for the lists: a rebuild is a
// deploy job, recognisable only by its payload.
func jobLine(j Job) string {
	var p tileJob
	if j.Kind == string(kindDeploy) && json.Unmarshal([]byte(j.Payload), &p) == nil && p.Rebuild {
		return deploy.RebuildLine
	}
	return ""
}

// PollJob returns the job and its log from offset on, for a follow view.
func (o *Orchestrator) PollJob(ctx context.Context, id string, offset int64) (Job, JobLog, error) {
	return o.jobRows.Poll(ctx, id, offset)
}

// Jobs lists jobs in the given states (none = every state), newest first,
// at most limit (0 = no cap).
func (o *Orchestrator) Jobs(ctx context.Context, limit int, states ...string) ([]Job, error) {
	js, err := o.jobRows.Recent(ctx, limit, states...)
	if err != nil {
		return nil, err
	}
	return o.annotate(ctx, js)
}

// ScopeJobs lists the jobs of one org, narrowed to a stack and an env when
// given (ids), newest first, at most limit (0 = no cap). The org and stack
// are read off the payload stamped at enqueue; an env is placed by its
// tile, env or volume id.
// ponytail: scans every job row of the states; a SQL filter on the payload
// when an install holds enough jobs to feel it.
func (o *Orchestrator) ScopeJobs(ctx context.Context, orgID, stackID, envID string, limit int, states ...string) ([]Job, error) {
	all, err := o.jobRows.Recent(ctx, 0, states...)
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, j := range all {
		var p map[string]any
		_ = json.Unmarshal([]byte(j.Payload), &p)
		if s, _ := p["org_id"].(string); s != orgID {
			continue
		}
		if s, _ := p["stack_id"].(string); stackID != "" && s != stackID {
			continue
		}
		if envID != "" && o.jobEnv(ctx, p) != envID {
			continue
		}
		if out = append(out, j); limit > 0 && len(out) == limit {
			break
		}
	}
	return o.annotate(ctx, out)
}

// TileJobs is the newest jobs touching the tiles, newest first.
func (o *Orchestrator) TileJobs(ctx context.Context, tileIDs []string, limit int) ([]Job, error) {
	js, err := o.jobRows.History(ctx, tileIDs, limit)
	if err != nil {
		return nil, err
	}
	return o.annotate(ctx, js)
}

// CancelJob stops a queued, waiting or building job. Mid-swap is a
// Conflict, and so is an already finished job.
func (o *Orchestrator) CancelJob(ctx context.Context, id string) error {
	return o.jobs.Cancel(ctx, id)
}

// lockLog is the log a promote or env sync writes to: each "deploying <slug>"
// line first adds that tile to the job's lock set, so the tile's Deployments
// tab lists the job (job_tiles follows lock_set by trigger) and a tile the job
// created holds queued jobs. Only tiles the job deployed or attempted get in;
// the set was fixed at enqueue and a created tile is not in it, so its
// "created <slug>" line adds it too, before any deploy for it can start.
type lockLog struct {
	ctx   context.Context
	o     *Orchestrator
	r     *jobs.Run
	envID string
}

func (l lockLog) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	slug, ok := strings.CutPrefix(line, "deploying ")
	if !ok { // a tile the promote created holds the lock from the line that says so
		slug, ok = strings.CutPrefix(line, "created ")
	}
	if ok {
		if t, err := l.o.tiles.GetBySlug(l.ctx, l.envID, slug); err == nil {
			if err := l.r.Lock(l.ctx, t.ID); err != nil {
				slog.Error("recording the job's tile", "job", l.r.Job.ID, "error", err)
			}
		}
	}
	return l.r.Log.Write(p)
}

// StoppedAt is the slug of the tile a failed promote stopped at, "" when it
// recorded none.
func StoppedAt(j Job) string {
	var p struct {
		StoppedAt string `json:"stopped_at"`
	}
	_ = json.Unmarshal([]byte(j.Payload), &p)
	return p.StoppedAt
}
