package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/upgrade"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// upgradeRolledBackMsg is the failed upgrade job's error and the Update tab's
// line for it.
const upgradeRolledBackMsg = "Rolled back: the new panel did not pass its trial."

// undoFailedUpgrade runs at boot, before anything is published. A helper that
// exited non-zero, or exited 0 with this panel not at the target version (a
// clean rollback), left this (old) panel in place: the upgrade put stackr-proxy
// on the target image and the new panel's database is gone, so it puts the
// proxy back on this panel's image, drops the target's tag from the cluster
// registry and the worker rollout aimed at it, and fails the upgrade job (it
// read done: the old panel ended it when it launched the helper). The helper
// is removed after, so a later boot does not repeat this.
// It first waits for a running helper to finish, so a panel booted by the swap
// publishes (and rolls out) only once it has passed its trial.
func (o *Orchestrator) undoFailedUpgrade(ctx context.Context) {
	ctx, err := o.withController(ctx)
	if err != nil {
		return
	}
	cs, err := o.upgraderDone(ctx)
	if err != nil {
		slog.Warn("upgrade: helper outcome unknown", "err", err)
		return
	}
	for _, c := range cs {
		d, err := o.docker.Inspect(ctx, c.ID)
		// The helper runs the target's image (installer and panel alike), so
		// its tag is the trial's target, whatever the job table says.
		target := ""
		if i := strings.LastIndex(d.Image, ":"); i >= 0 && !strings.Contains(d.Image[i:], "/") {
			target = d.Image[i+1:]
		}
		if err != nil || d.State != "exited" || (d.ExitCode == 0 && !o.cleanRollback(target)) {
			continue
		}
		logs, _ := o.docker.Logs(ctx, c.ID, 20)
		slog.Warn("upgrade: the new panel failed its trial, rolling back", "helper", logs)
		// Kept on an error: the helper is the marker the next boot retries from.
		if err := o.rollBackUpgrade(ctx, target); err != nil {
			slog.Warn("upgrade rollback: not finished, retried at next boot", "err", err)
			continue
		}
		if err := o.docker.StopRemove(ctx, c.ID); err != nil {
			slog.Warn("upgrade helper: not removed", "err", err)
		}
	}
}

// cleanRollback: a helper that exited 0 after a clean rollback (the on-failure
// restart policy must not rerun it) left this panel in place, not the
// upgrade's target. Exit 0 with this panel AT the target is a pass. A dev
// build or an unknown target is never a rollback.
func (o *Orchestrator) cleanRollback(target string) bool {
	v := strings.TrimPrefix(o.cfg.Version, "v")
	return v != "" && v != "dev" && target != "" && strings.TrimPrefix(target, "v") != v
}

// rollBackUpgrade undoes the failed trial of target (the helper's image tag):
// its effects, and the upgrade job for that tag if the panel ran one (the
// installer writes none). Other jobs are left alone.
func (o *Orchestrator) rollBackUpgrade(ctx context.Context, target string) error {
	// done: as the old panel left it; failed: a restored database that still
	// had it running, which boot fails as interrupted.
	js, err := o.store.Jobs.ListByState(ctx, "done", "failed") // newest first
	if err != nil {
		return err
	}
	for _, j := range js {
		if j.Kind != string(kindUpgrade) {
			continue
		}
		var p upgradeJob
		_ = json.Unmarshal([]byte(j.Payload), &p)
		if target != "" && strings.TrimPrefix(p.Tag, "v") == strings.TrimPrefix(target, "v") {
			return errors.Join(o.undoUpgradeEffects(ctx, target), o.jobRows.Finish(ctx, j, job.Failed, upgradeRolledBackMsg))
		}
	}
	return o.undoUpgradeEffects(ctx, target)
}

// undoUpgradeEffects puts back what the upgrade changed before the swap:
// stackr-proxy (Flow.EnsureProxy), and what the target's boot publish put in
// the registry and aimed the workers at.
func (o *Orchestrator) undoUpgradeEffects(ctx context.Context, tag string) error {
	img, err := o.runningPanelImage(ctx)
	if err == nil {
		err = o.upgrade.EnsureProxy(ctx, img)
	}
	// Never the running version's own image: it is what the workers pull.
	if tag == "" || strings.TrimPrefix(tag, "v") == strings.TrimPrefix(o.cfg.Version, "v") {
		return err
	}
	ref := stackrTagRef(upgrade.ImageRef(tag))
	t := ref[strings.LastIndex(ref, ":")+1:]
	stackrDigests.Delete(t)
	dg, rerr := o.regs.Resolve(ctx, stackrRepo, t)
	if rerr == nil && dg != "" {
		rerr = o.regs.Delete(ctx, stackrRepo, dg)
	}
	return errors.Join(err, rerr, o.store.Settings.Delete(ctx, keyRolloutTarget))
}

// LastUpgradeError is the Update tab's line for the newest upgrade job, when
// it failed ("" = none, still going, or it worked).
func (o *Orchestrator) LastUpgradeError(ctx context.Context) string {
	js, err := o.store.Jobs.ListByState(ctx, "queued", "running", "waiting", "done", "failed") // newest first
	if err != nil {
		return ""
	}
	for _, j := range js {
		if j.Kind == string(kindUpgrade) {
			if j.State == job.Failed {
				return j.Error
			}
			return ""
		}
	}
	return ""
}
