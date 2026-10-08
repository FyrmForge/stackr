package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The server file's routes are the admin's: a plan from a file, the list, the
// approve (which records who approved, so the apply can make them owner of
// the orgs it creates), the reject and the export.
func TestServerConfigAdminFlow(t *testing.T) {
	w := newWorld(t)
	const planFile = `{"file":"version: 1\nroutes: []\n"}`
	if code, _ := w.do(t, w.owner, "POST", "/admin/config/plan-file", planFile); code != 403 {
		t.Errorf("an org owner planning the server file = %d, want 403", code)
	}
	if code, out := w.do(t, w.admin, "POST", "/admin/config/plan-file", `{"file":"version: 9\n"}`); code != 400 {
		t.Errorf("a bad file = %d %s, want 400", code, out)
	}
	code, out := w.do(t, w.admin, "POST", "/admin/config/plan-file", planFile)
	var plan struct {
		ID, Source, Status string
	}
	if err := json.Unmarshal([]byte(out), &plan); code != 200 || err != nil || plan.Source != "local" || plan.Status != "clean" {
		t.Fatalf("plan-file = %d %s (%v); want a clean local plan", code, out, err)
	}
	code, out = w.do(t, w.admin, "GET", "/admin/config/plans", "")
	if code != 200 || !strings.Contains(out, plan.ID) || strings.Contains(out, `"file"`) {
		t.Errorf("plans = %d %s; want the plan and not its file bytes", code, out)
	}

	// a pending plan: approve records the approver on the job
	code, out = w.do(t, w.admin, "POST", "/admin/config/plan-file", `{"file":"version: 1\nroutes:\n  - host: a.example.com\n    mode: https\n    target: b.internal\n"}`)
	if code != 200 {
		t.Fatalf("plan-file = %d %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil || plan.Status != "pending" {
		t.Fatalf("plan = %s (%v); want pending", out, err)
	}
	code, out = w.do(t, w.admin, "POST", "/admin/config/plans/"+plan.ID+"/approve", "")
	var job struct{ Kind, Payload string }
	if err := json.Unmarshal([]byte(out), &job); code != 202 || err != nil || job.Kind != "server-apply" {
		t.Fatalf("approve = %d %s (%v)", code, out, err)
	}
	var pl struct {
		PlanID     string `json:"plan_id"`
		ApproverID string `json:"approver_id"`
	}
	_, me := w.do(t, w.admin, "GET", "/me", "")
	if err := json.Unmarshal([]byte(job.Payload), &pl); err != nil || pl.PlanID != plan.ID ||
		pl.ApproverID == "" || !strings.Contains(me, pl.ApproverID) {
		t.Errorf("apply payload = %s; want the plan and the approving admin (%s)", job.Payload, me)
	}

	if code, out = w.do(t, w.admin, "POST", "/admin/config/plan-file", planFile); code != 200 {
		t.Fatalf("plan-file = %d %s", code, out)
	}
	if code, out := w.do(t, w.admin, "GET", "/admin/config/export", ""); code != 200 || !strings.Contains(out, "version: 1") {
		t.Errorf("export = %d %s", code, out)
	}
}

// The generic settings PUT does not write the server file's binding: bind
// checks the connector, this would not.
func TestSettingRefusesServerConfigKnobs(t *testing.T) {
	w := newWorld(t)
	if code, out := w.do(t, w.admin, "PUT", "/admin/settings/server_config_repo", `{"value":"acme/server"}`); code != 400 || !strings.Contains(out, "config-repo") {
		t.Errorf("PUT server_config_repo = %d %s, want 400 pointing at bind", code, out)
	}
	if code, out := w.do(t, w.admin, "PUT", "/admin/settings/cleanup_schedule", `{"value":"0 3 * * *"}`); code != 204 && code != 200 {
		t.Errorf("PUT cleanup_schedule = %d %s", code, out)
	}
}
