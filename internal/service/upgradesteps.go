package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/upgrade"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The upgrade's steps in order, as UpgradeProgress.Step counts them. 0 is no
// step (idle, or queued behind running jobs); UpgradeDone is every step done
// while the helper finishes.
const (
	UpgradePull = iota + 1
	UpgradeArchive
	UpgradeRestart
	UpgradeCheck
	UpgradeDone
)

// keyUpgradeFailure holds the newest failed upgrade's UpgradeFailure as JSON.
const keyUpgradeFailure = "upgrade_failure"

// UpgradeProgress is where the running upgrade is, for the Update tab.
type UpgradeProgress struct {
	Step    int           // UpgradePull .. UpgradeDone; 0 = none
	Target  string        // the release it moves to
	Elapsed time.Duration // since the job started
}

// UpgradeFailure is why the last upgrade rolled back: the new panel's exit
// code and the tail of its log, kept by the helper before it removed it.
type UpgradeFailure struct {
	Tag      string `json:"tag"`
	Reason   string `json:"reason"`
	ExitCode int    `json:"exit_code"`
	Log      string `json:"log"`
}

func (o *Orchestrator) setUpgradeStep(s string) {
	o.upgradeStep.Store(int32(map[string]int{
		upgrade.StepPull: UpgradePull, upgrade.StepArchive: UpgradeArchive, upgrade.StepSwap: UpgradeRestart,
	}[s]))
}

// newestUpgrade is the newest upgrade job in any state.
func (o *Orchestrator) newestUpgrade(ctx context.Context) (store.Job, upgradeJob, bool) {
	// an upgrade holds every tile's lock; only its kind is read, the newest few
	js, err := o.store.Jobs.ListTouching(ctx, []string{job.All}, string(kindUpgrade), 10) // newest first
	if err != nil {
		return store.Job{}, upgradeJob{}, false
	}
	i := slices.IndexFunc(js, func(j store.Job) bool {
		return slices.Contains([]string{job.Queued, job.Running, job.Waiting, job.Done, job.Failed}, j.State)
	})
	if i < 0 {
		return store.Job{}, upgradeJob{}, false
	}
	var p upgradeJob
	_ = json.Unmarshal([]byte(js[i].Payload), &p)
	return js[i], p, true
}

// UpgradeProgress is the step the running upgrade is on. The old panel knows
// its own (the flow reports them); once the helper has stopped it, the new
// panel only knows the helper still runs: every step is done when this build
// is the target, otherwise it is still checking the trial.
func (o *Orchestrator) UpgradeProgress(ctx context.Context) UpgradeProgress {
	j, p, ok := o.newestUpgrade(ctx)
	if !ok {
		return UpgradeProgress{}
	}
	out := UpgradeProgress{Target: p.Tag}
	switch step := int(o.upgradeStep.Load()); {
	case step > 0:
		out.Step = step
	case o.UpgradeHeld():
		out.Step = UpgradeCheck
		if o.upgradeLanded(j) {
			out.Step = UpgradeDone
		}
	case j.State == job.Running:
		out.Step = UpgradePull
	}
	if out.Step > 0 && j.StartedAt != nil {
		out.Elapsed = time.Since(*j.StartedAt)
	}
	return out
}

// LastUpgradeFailure is why the newest upgrade rolled back; false = none kept.
func (o *Orchestrator) LastUpgradeFailure(ctx context.Context) (UpgradeFailure, bool) {
	var f UpgradeFailure
	raw, ok, err := o.store.Settings.Get(ctx, keyUpgradeFailure)
	if err != nil || !ok || json.Unmarshal([]byte(raw), &f) != nil {
		return f, false
	}
	return f, true
}

// keepUpgradeFailure stores what the helper left for the trial of tag, so the
// log outlives the container it came from.
func (o *Orchestrator) keepUpgradeFailure(ctx context.Context, tag string) {
	pf, ok := panel.ReadFailure(o.cfg.DBPath, tag)
	if !ok {
		return
	}
	b, err := json.Marshal(UpgradeFailure{Tag: tag, Reason: pf.Reason, ExitCode: pf.ExitCode, Log: pf.Log})
	if err == nil {
		err = o.store.Settings.Set(ctx, keyUpgradeFailure, string(b))
	}
	if err != nil {
		slog.Warn("upgrade: failure not kept", "err", err)
		return // the file stays for the next boot's retry
	}
	panel.RemoveFailure(o.cfg.DBPath)
}
