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
		rs, err := w.env.O.Releases(ctx, w.tile.Stack)
		if err == nil && len(rs) == 1 {
			rel = rs[0]
		}
		return rel.ID != ""
	})
	// shop's release aimed at blog/dev is blocked.
	st, err := w.env.O.CreateStack(ctx, w.acme, "blog", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.O.CreateEnv(ctx, st.ID, "dev", service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	const env = "/orgs/acme/stacks/blog/envs/dev"

	code, body := w.do(t, w.owner, "GET", env+"/plan/"+rel.ID, "")
	var pp service.PromotePlan
	if err := json.Unmarshal([]byte(body), &pp); err != nil || code != 200 {
		t.Fatalf("plan = %d %s", code, body)
	}
	if pp.CanDeploy || len(pp.Plan.Blockers) == 0 {
		t.Fatalf("plan = %s, want blocked", body)
	}
	want := strings.Join(pp.Plan.Blockers, "; ")

	for _, verb := range []string{"promote", "rollback"} {
		code, body := w.do(t, w.owner, "POST", env+"/"+verb+"/"+rel.ID, "")
		var j service.Job
		if err := json.Unmarshal([]byte(body), &j); err != nil || code != 202 {
			t.Fatalf("%s = %d %s", verb, code, body)
		}
		eventually(t, verb+" to finish", func() bool {
			j, err = w.env.O.GetJob(ctx, j.ID)
			return err == nil && j.FinishedAt != nil
		})
		if j.State != "failed" || !strings.Contains(j.Error, want) {
			t.Errorf("%s = %s %q, want failed with %q", verb, j.State, j.Error, want)
		}
	}
}
