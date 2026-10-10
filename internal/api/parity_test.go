package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// B2, B20: the dry run names what blocks a promote in one field, and the
// real promote and a rollback both refuse with exactly that text: one verb,
// one rule, whoever calls.
func TestPromoteParity(t *testing.T) {
	w, conn := hookWorld(t)
	ctx := context.Background()
	if code := w.hook(t, conn, "push", "whsec", push); code != 204 {
		t.Fatalf("push = %d", code)
	}
	var rel service.Release
	eventually(t, "a release", func() bool {
		rs, err := w.env.Orch.Releases(ctx, w.tile.Stack)
		if err == nil && len(rs) == 1 {
			rel = rs[0]
		}
		return rel.ID != ""
	})
	// shop's release aimed at a static env of the same stack is blocked.
	var err error
	if _, err = w.env.Orch.CreateEnv(
		ctx,
		w.tile.Stack,
		"qa",
		service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"},
	); err != nil {
		t.Fatal(err)
	}
	const env = "/orgs/acme/stacks/shop/envs/qa"

	code, body := w.do(t, w.owner, "GET", env+"/plan/"+rel.ID, "")
	var pp service.PromotePlan
	if err := json.Unmarshal([]byte(body), &pp); err != nil || code != 200 {
		t.Fatalf("plan = %d %s", code, body)
	}
	if pp.CanDeploy || len(pp.Plan.Blockers) == 0 {
		t.Fatalf("plan = %s, want blocked", body)
	}
	want := strings.Join(pp.Plan.Blockers, "; ")

	// a release of another stack is a 404 on every verb
	w.env.Stack(t, w.acme, "web")
	for _, verb := range []string{"plan", "promote", "rollback"} {
		m := "POST"
		if verb == "plan" {
			m = "GET"
		}
		if code, _ := w.do(t, w.owner, m, "/orgs/acme/stacks/web/envs/dev/"+verb+"/"+rel.ID, ""); code != 404 {
			t.Errorf("foreign release %s = %d, want 404", verb, code)
		}
	}

	// a promote body's ticked keys ride into the job
	if code, body := w.do(t, w.owner, "POST", env+"/promote/"+rel.ID, `{"ticked":["param:app.pw"]}`); code != 202 {
		t.Fatalf("promote with ticked = %d %s", code, body)
	}
	js, _ := w.env.Orch.Jobs(ctx, 20)
	var got bool
	for _, j := range js {
		got = got || strings.Contains(j.Payload, `"ticked":["param:app.pw"]`)
	}
	if !got {
		t.Errorf("no promote job carries the ticks: %+v", js)
	}

	for _, verb := range []string{"promote", "rollback"} {
		code, body := w.do(t, w.owner, "POST", env+"/"+verb+"/"+rel.ID, "")
		var j service.Job
		if err := json.Unmarshal([]byte(body), &j); err != nil || code != 202 {
			t.Fatalf("%s = %d %s", verb, code, body)
		}
		eventually(t, verb+" to finish", func() bool {
			j, err = w.env.Orch.GetJob(ctx, j.ID)
			return err == nil && j.FinishedAt != nil
		})
		if j.State != "failed" || !strings.Contains(j.Error, want) {
			t.Errorf("%s = %s %q, want failed with %q", verb, j.State, j.Error, want)
		}
	}
}
