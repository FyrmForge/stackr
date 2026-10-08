package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// A plan with no impact line is approved with no confirm; if the server then
// moves so that applying it would be a risky change (acme_email back to the
// file's value), the apply refuses and plans again instead of doing it
// unconfirmed.
func TestServerApplyRefusesNewRiskyChange(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.env.Orch.SetSetting(ctx, "acme_email", "a@example.com"))
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nsettings:\n  acme_email: a@example.com\nroutes:\n  - host: new.example.com\n    mode: https\n    target: backend.internal:8443\n"))
	must(err)
	if strings.Contains(pl.Plan, `"impact"`) {
		t.Fatalf("plan already has an impact line: %s", pl.Plan)
	}
	must(r.env.Orch.SetSetting(ctx, "acme_email", "b@example.com"))
	_, err = r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{})
	must(err)
	var last service.ServerPlan
	eventually(t, "the apply to end", func() bool {
		last, _ = r.env.Orch.ServerPlan(ctx, pl.ID)
		return last.Status == "applied" || last.Status == "error"
	})
	if last.Status != "error" || !strings.Contains(last.Error, "plan again") {
		t.Fatalf("plan = %s %q, want an error asking to plan again", last.Status, last.Error)
	}
	if v, _ := r.env.Orch.Setting(ctx, "acme_email"); v != "b@example.com" {
		t.Errorf("acme_email = %q, the refused apply changed it", v)
	}
	if got := r.routes(t); len(got) != 0 {
		t.Errorf("routes = %v, the refused apply walked", got)
	}
}

// An org whose slug is "all" (data from before the slug was reserved) is one
// org: dropping it from a connector's shares takes only that org out, and
// never reads as leaving share-all.
func TestServerUnshareOrgNamedAll(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	acme, all := r.env.Org(t, "acme"), r.env.Org(t, "all")
	if _, err := r.env.Orch.ShareConnector(ctx, r.conn, []string{acme, all}, false); err != nil {
		t.Fatal(err)
	}
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nconnectors:\n  - name: gh\n    share:\n      - acme\n"))
	if err != nil || !strings.Contains(pl.Plan, `"key":"connector-share:gh/org:all"`) {
		t.Fatalf("plan = %+v, %v; want the org removal row", pl, err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"connector-share:gh/org:all"}}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	scs, err := r.env.Orch.ServerConnectors(ctx)
	if err != nil || len(scs) != 1 || scs[0].ShareAll || len(scs[0].OrgIDs) != 1 || scs[0].OrgIDs[0] != acme {
		t.Errorf("connectors = %+v, %v; want shared with acme only", scs, err)
	}
}

// Leaving share-all falls back to the file's list.
func TestServerLeaveShareAll(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	acme := r.env.Org(t, "acme")
	if _, err := r.env.Orch.ShareConnector(ctx, r.conn, nil, true); err != nil {
		t.Fatal(err)
	}
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nconnectors:\n  - name: gh\n    share:\n      - acme\n"))
	if err != nil || !strings.Contains(pl.Plan, `"key":"connector-share:gh/all"`) {
		t.Fatalf("plan = %+v, %v; want the share-all removal row", pl, err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"connector-share:gh/all"}}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	scs, err := r.env.Orch.ServerConnectors(ctx)
	if err != nil || len(scs) != 1 || scs[0].ShareAll || len(scs[0].OrgIDs) != 1 || scs[0].OrgIDs[0] != acme {
		t.Errorf("connectors = %+v, %v; want shared with acme only", scs, err)
	}
}
