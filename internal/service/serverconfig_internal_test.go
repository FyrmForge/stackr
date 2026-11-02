package service

import (
	"encoding/json"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"testing"
)

// Both server job kinds have a handler (a kind without one fails with "no
// handler"), and the apply payload keeps its plan id key.
func TestServerJobKinds(t *testing.T) {
	var o Orchestrator
	hs := o.handlers()
	for _, k := range []string{string(kindServerPlan), string(kindServerApply)} {
		found := false
		for kind := range hs {
			found = found || string(kind) == k
		}
		if !found {
			t.Errorf("no handler for job kind %q", k)
		}
	}
	if kindServerPlan != "server-plan" || kindServerApply != "server-apply" {
		t.Errorf("kinds = %q, %q", kindServerPlan, kindServerApply)
	}
	b, err := json.Marshal(serverApplyJob{PlanID: "p1"})
	if err != nil || string(b) != `{"plan_id":"p1"}` {
		t.Errorf("apply payload = %s, %v", b, err)
	}
	if b, _ := json.Marshal(serverPlanJob{}); string(b) != `{}` {
		t.Errorf("plan payload = %s", b)
	}
}

// The node verbs' job kinds have a handler: a queued remove, node backup or
// worker rollout would otherwise fail with "no handler for job kind".
func TestNodeJobKindsHaveHandlers(t *testing.T) {
	var o Orchestrator
	hs := o.handlers()
	for _, k := range []jobs.Kind{kindRemoveNode, kindNodeBackup, kindWorkerRollout} {
		if hs[k] == nil {
			t.Errorf("no handler for job kind %q", k)
		}
	}
}

// An apply refuses any row the approved plan did not show, and a risky row
// the approver did not confirm.
func TestNewRisk(t *testing.T) {
	was := planfile.Plan{Changes: []planfile.Change{
		{Kind: "settings", Field: "workers"},
		{Kind: "settings", Field: "acme_email", Impact: "certs"},
	}}
	b, _ := json.Marshal(was)
	now := planfile.Plan{Changes: []planfile.Change{{Kind: "settings", Field: "workers"}}}
	if err := newRisk(string(b), false, now); err != nil {
		t.Errorf("same rows: %v", err)
	}
	now.Changes = append(now.Changes, planfile.Change{Kind: "settings", Field: "log_level"})
	if err := newRisk(string(b), true, now); err == nil {
		t.Error("a panel edit since the plan applied silently")
	}
	now.Changes = []planfile.Change{{Kind: "settings", Field: "acme_email", Impact: "certs"}}
	if err := newRisk(string(b), false, now); err == nil {
		t.Error("an unconfirmed risky row applied")
	}
	if err := newRisk(string(b), true, now); err != nil {
		t.Errorf("a confirmed risky row: %v", err)
	}
}
