package service

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
)

// configState keeps the two config problems of a scope ("server" or an org
// id) in step with its newest plan: invalid (a file that does not fetch or
// parse; alerts once, until it parses again) and drift (changes waiting for
// approval, which includes a panel edit the file does not hold). status is
// the plan row's.
func (o *Orchestrator) configState(ctx context.Context, scope, subject, status, errMsg, summary string) {
	bad, drift := "alert.config.invalid."+scope, "alert.config.drift."+scope
	switch status {
	case orgplan.Error:
		o.clearProblem(ctx, drift)
		o.openProblem(ctx, bad, SevWarning, subject+" is invalid: "+errMsg)
		if _, was, err := o.store.Settings.Get(ctx, bad); err != nil || was {
			return
		}
		if o.store.Settings.Set(ctx, bad, time.Now().UTC().Format(time.RFC3339)) == nil {
			o.alert(Alert{Event: AlertConfigBad, Severity: SevWarning, Subject: scope,
				Message: subject + " is invalid: " + errMsg})
		}
	case orgplan.Pending:
		o.clearConfigInvalid(ctx, bad)
		o.openProblem(ctx, drift, SevWarning, subject+" differs from the install: "+summary)
	default:
		o.clearConfigInvalid(ctx, bad)
		o.clearProblem(ctx, drift)
	}
}

func (o *Orchestrator) clearConfigInvalid(ctx context.Context, key string) {
	_ = o.store.Settings.Delete(ctx, key)
	o.clearProblem(ctx, key)
}

// replanConfigs queues a fresh plan of every bound config file, so a panel
// edit the file does not hold surfaces as a plan (and the drift problem)
// without waiting for the next push. Called from dailyChores.
func (o *Orchestrator) replanConfigs(ctx context.Context) {
	if b, err := o.ServerConfigBinding(ctx); err == nil && b.Repo != "" {
		_, _ = o.enqueue(ctx, kindServerPlan, serverPlanJob{}, "serverconfig")
	}
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return
	}
	for _, og := range ogs {
		if og.ConfigRepo != "" {
			_, _ = o.enqueue(ctx, kindOrgPlan, orgPlanJob{OrgID: og.ID}, "orgconfig:"+og.ID)
		}
	}
}
