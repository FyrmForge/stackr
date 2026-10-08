package service_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// The binding is the five server_config_* settings, read as one struct.
func TestServerConfigBinding(t *testing.T) {
	orch, ctx := servicetest.New(t).Orch, context.Background()
	if b, err := orch.ServerConfigBinding(ctx); err != nil || b != (service.ServerBinding{}) {
		t.Fatalf("fresh binding = %+v, %v; want unbound", b, err)
	}
	for k, v := range map[string]string{
		"server_config_connector": "c1",
		"server_config_repo":      "acme/server",
		"server_config_branch":    "prod",
		"server_config_path":      "infra/stackr-server.yml",
		"server_config_auto":      "true",
	} {
		if err := orch.SetSettings(ctx, map[string]string{k: v}); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	want := service.ServerBinding{
		ConnectorID: "c1",
		Repo:        "acme/server",
		Branch:      "prod",
		Path:        "infra/stackr-server.yml",
		Auto:        true,
	}
	if b, err := orch.ServerConfigBinding(ctx); err != nil || b != want {
		t.Errorf("binding = %+v, %v; want %+v", b, err, want)
	}
}

// seedServerPlan stores a pending server plan row.
func seedServerPlan(t *testing.T, env *servicetest.Env, plan string, age time.Duration) service.ServerPlan {
	t.Helper()
	p := service.ServerPlan{
		ID: uuid.NewString(), Plan: plan, Status: "pending", Source: "repo",
		Ticked: []string{}, CreatedAt: time.Now().Add(-age),
	}
	if err := env.Store.ServerPlans.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

// The plans list is newest first and capped; a missing id is not found.
func TestServerPlansListAndGet(t *testing.T) {
	env, ctx := servicetest.New(t), context.Background()
	old := seedServerPlan(t, env, `{}`, 2*time.Hour)
	fresh := seedServerPlan(t, env, `{}`, time.Hour)
	ps, err := env.Orch.ServerPlans(ctx, 1)
	if err != nil || len(ps) != 1 || ps[0].ID != fresh.ID {
		t.Fatalf("ServerPlans(1) = %+v, %v; want the newest only", ps, err)
	}
	if ps, _ = env.Orch.ServerPlans(ctx, 10); len(ps) != 2 || ps[1].ID != old.ID {
		t.Errorf("ServerPlans(10) = %+v; want newest first", ps)
	}
	if p, err := env.Orch.ServerPlan(ctx, old.ID); err != nil || p.ID != old.ID {
		t.Errorf("ServerPlan = %+v, %v", p, err)
	}
	if _, err := env.Orch.ServerPlan(ctx, "nope"); err == nil {
		t.Error("ServerPlan of a missing id passed")
	}
}

// Reject ends a pending plan once; an approved one is not rejectable.
func TestRejectServerPlan(t *testing.T) {
	env, ctx := servicetest.New(t), context.Background()
	p := seedServerPlan(t, env, `{}`, 0)
	got, err := env.Orch.RejectServerPlan(ctx, p.ID)
	if err != nil || got.Status != "rejected" || got.DecidedAt == nil {
		t.Fatalf("RejectServerPlan = %+v, %v", got, err)
	}
	if _, err := env.Orch.RejectServerPlan(ctx, p.ID); err == nil {
		t.Error("a second reject passed")
	}
}

// A server approve is the org one: impact lines need the confirm, a tick must
// name a removal row, both land on the row, and the apply job is queued with
// the approver and the locks (the plan, the server, each org that exists).
func TestApproveServerPlanContract(t *testing.T) {
	env, ctx := servicetest.New(t), context.Background()
	acme := env.Org(t, "acme")
	risky := seedServerPlan(t, env, `{"changes":[`+
		`{"kind":"settings","field":"panel_domain","impact":"panel moves to x"},`+
		`{"kind":"org-create","tile":"newco"},`+
		`{"kind":"org-binding","tile":"acme"},`+
		`{"kind":"route-delete","tile":"old.example.com","key":"route:old.example.com","optional":true}]}`, 0)

	_, err := env.Orch.ApproveServerPlan(ctx, risky.ID, service.ApproveOpts{})
	if !isConflict(err) || !strings.Contains(err.Error(), "This plan has impact lines; confirm to approve.") {
		t.Fatalf("unconfirmed approve = %v, want the impact refusal", err)
	}
	if _, err = env.Orch.ApproveServerPlan(ctx, risky.ID, service.ApproveOpts{Confirm: true, Ticked: []string{"dest:x"}}); err == nil {
		t.Fatal("a tick outside the plan's removals passed")
	}
	if p, _ := env.Orch.ServerPlan(ctx, risky.ID); p.DecidedAt != nil {
		t.Fatal("a refused approve stamped the row")
	}
	j, err := env.Orch.ApproveServerPlan(ctx, risky.ID, service.ApproveOpts{
		Confirm:    true,
		Ticked:     []string{"route:old.example.com"},
		ApproverID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.Kind != "server-apply" {
		t.Errorf("job kind = %q", j.Kind)
	}
	var pl map[string]any
	if err := json.Unmarshal([]byte(j.Payload), &pl); err != nil ||
		pl["plan_id"] != risky.ID || pl["approver_id"] != "u1" || pl["org_id"] != nil {
		t.Errorf("payload = %s (%v); want plan_id and approver_id, no org_id", j.Payload, err)
	}
	for _, want := range []string{"serverconfig", "serverplan:" + risky.ID, "orgconfig:" + acme} {
		if !slices.Contains(j.LockSet, want) {
			t.Errorf("lock set %v lacks %q", j.LockSet, want)
		}
	}
	if len(j.LockSet) != 3 {
		t.Errorf("lock set %v; want serverconfig, the plan and the one org that exists", j.LockSet)
	}
	p, err := env.Orch.ServerPlan(ctx, risky.ID)
	if err != nil || p.DecidedAt == nil || !p.Confirmed || len(p.Ticked) != 1 || p.Ticked[0] != "route:old.example.com" {
		t.Errorf("approved row = %+v, %v; want the tick and the confirm stored", p, err)
	}
	if _, err := env.Orch.ApproveServerPlan(ctx, risky.ID, service.ApproveOpts{Confirm: true}); err == nil {
		t.Error("a second approve passed")
	}

	failed := seedServerPlan(t, env, ``, 0)
	failed.Status, failed.Error = "error", "git clone: exit status 128"
	if err := env.Store.ServerPlans.Update(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ApproveServerPlan(ctx, failed.ID, service.ApproveOpts{}); !isConflict(err) {
		t.Errorf("approve of an error row = %v, want Conflict", err)
	}

	blocked := seedServerPlan(t, env, `{"changes":[],"blockers":["backup dest offsite needs server.params.s3.secret_key"]}`, 0)
	if _, err := env.Orch.ApproveServerPlan(ctx, blocked.ID, service.ApproveOpts{}); !isConflict(err) {
		t.Errorf("a blocked plan = %v, want a Conflict", err)
	}
}

// A push on the bound repo and branch, through the server's connector,
// queues a server plan; anything else queues nothing.
func TestQueueServerPlan(t *testing.T) {
	env, ctx := servicetest.New(t), context.Background()
	for k, v := range map[string]string{
		"server_config_connector": "c1",
		"server_config_repo":      "acme/server",
		"server_config_branch":    "prod",
	} {
		if err := env.Orch.SetSettings(ctx, map[string]string{k: v}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name                         string
		conn, repo, branch, def, got string
	}{
		{"match", "c1", "https://github.com/Acme/server.git", "prod", "main", "server-plan"},
		{"other branch", "c1", "acme/server", "dev", "main", ""},
		{"other repo", "c1", "acme/other", "prod", "main", ""},
		{"other connector", "c2", "acme/server", "prod", "main", ""},
	} {
		j, err := env.Orch.QueueServerPlan(ctx, c.conn, c.repo, c.branch, c.def)
		if err != nil || j.Kind != c.got {
			t.Errorf("%s: job %q, %v; want %q", c.name, j.Kind, err, c.got)
		}
		if c.got != "" && !slices.Equal(j.LockSet, []string{"serverconfig"}) {
			t.Errorf("%s: lock set %v", c.name, j.LockSet)
		}
	}
	// no bound branch = the repo's default one
	if err := env.Orch.SetSettings(ctx, map[string]string{"server_config_branch": ""}); err != nil {
		t.Fatal(err)
	}
	if j, err := env.Orch.QueueServerPlan(ctx, "c1", "acme/server", "main", "main"); err != nil || j.Kind != "server-plan" {
		t.Errorf("push to the default branch: %q, %v", j.Kind, err)
	}
	if j, err := env.Orch.QueueServerPlan(ctx, "c1", "acme/server", "dev", "main"); err != nil || j.Kind != "" {
		t.Errorf("push to another branch with none bound: %q, %v", j.Kind, err)
	}
}

// The binding takes a server connector only, and an empty repo unbinds: the
// five settings clear and the plans nobody approved are rejected.
func TestBindServerConfig(t *testing.T) {
	env, ctx := servicetest.New(t), context.Background()
	orgConn := env.Connector(t, env.Org(t, "acme"), "whsec")

	for name, conn := range map[string]string{"missing": "nope", "an org's": orgConn} {
		if _, err := env.Orch.BindServerConfig(ctx, conn, "acme/server", "", "", false); err == nil {
			t.Errorf("binding %s connector passed", name)
		}
	}
	if b, _ := env.Orch.ServerConfigBinding(ctx); b != (service.ServerBinding{}) {
		t.Fatalf("a refused bind wrote %+v", b)
	}
	if _, err := env.Orch.BindServerConfig(ctx, "", "acme/server", "", "", false); err == nil {
		t.Error("a bind with no connector passed")
	}

	// bound by hand, as a prior bind leaves it
	for k, v := range map[string]string{
		"server_config_connector": "c1", "server_config_repo": "acme/server", "server_config_auto": "true",
	} {
		if err := env.Orch.SetSettings(ctx, map[string]string{k: v}); err != nil {
			t.Fatal(err)
		}
	}
	pending := seedServerPlan(t, env, `{}`, 0)
	b, err := env.Orch.BindServerConfig(ctx, "", "", "", "", false)
	if err != nil || b != (service.ServerBinding{}) {
		t.Fatalf("unbind = %+v, %v", b, err)
	}
	if got, _ := env.Orch.ServerConfigBinding(ctx); got != (service.ServerBinding{}) {
		t.Errorf("binding after unbind = %+v", got)
	}
	if p, _ := env.Orch.ServerPlan(ctx, pending.ID); p.Status != "rejected" {
		t.Errorf("pending plan after unbind = %s, want rejected", p.Status)
	}
}
