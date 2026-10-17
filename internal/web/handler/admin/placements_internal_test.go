package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

func TestPlacementState(t *testing.T) {
	run := &service.PlacementMoveStatus{State: "running", Done: 1, All: 3}
	for _, c := range []struct {
		name string
		p    service.Placement
		word string
		tone string
	}{
		{"online", service.Placement{NodeID: "n", Online: true}, "online", "success"},
		{"offline", service.Placement{NodeID: "n"}, "offline", "danger"},
		{"no node", service.Placement{}, "no node yet", "warn"},
		{"forgotten", service.Placement{NodeID: "gone", NodeForgotten: true}, "node forgotten: repoint to restore", "danger"},
		{"moving", service.Placement{NodeID: "n", Online: true, Moving: run}, "moving 1 of 3", "warn"},
		{"failed", service.Placement{NodeID: "n", Online: true, Moving: &service.PlacementMoveStatus{State: "failed"}}, "move failed", "danger"},
	} {
		if w, tone := placementState(c.p); w != c.word || tone != c.tone {
			t.Errorf("%s: %q %q, want %q %q", c.name, w, tone, c.word, c.tone)
		}
	}
}

// The open-to cell and the edit form agree on which orgs are on.
func TestPlacementRowOpen(t *testing.T) {
	ogs := []service.Org{{Slug: "acme", Name: "Acme"}, {Slug: "beta", Name: "Beta"}}
	r := placementRow(service.Placement{Name: "eu-db", Orgs: []string{"acme"}}, ogs)
	if r.Open != "1 org" || r.Delete.Action == "" || !r.Edit.Orgs[0].On || r.Edit.Orgs[1].On {
		t.Errorf("row = %+v", r)
	}
	if r := placementRow(service.Placement{Name: "default", Default: true, OpenToAll: true}, ogs); r.Open != "all orgs" || r.Delete.Action != "" {
		t.Errorf("default row = %+v", r)
	}
}

func plan(t *testing.T, js string) service.PlacementMovePlan {
	t.Helper()
	var p service.PlacementMovePlan
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A finished env reads "moved", once.
func TestMoveProgressViewDoneRow(t *testing.T) {
	v := moveProgressView("eu", service.PlacementMoveStatus{State: "done", Envs: []service.MoveStatus{{Path: "acme/shop/prod"}}})
	if e := v.Envs[0]; e.State != "moved" || e.Step != "" {
		t.Errorf("done row = %+v", e)
	}
}

// Apply is offered only when the service and every line agree.
func TestMoveChecksViewGatesApply(t *testing.T) {
	const ok = `{"Placement":"eu-prod","From":"h3","To":"h4","OK":true,"Envs":[{"Path":"acme/shop/prod","Volumes":2,"Bytes":-1,"Downtime":"about 20 s"}],
		"Checks":[{"name":"target online","ok":true},{"name":"disk","ok":true,"detail":"h4 free 12 GB"}]}`
	w := moveChecksView(&ui.MoveWizardView{}, plan(t, ok))
	if !w.OK || w.Confirm.Action != "/-/admin/placements/eu-prod/move" || w.Confirm.Word != "eu-prod" ||
		!strings.Contains(w.Confirm.Vals, `"to":"h4"`) || !strings.Contains(w.Confirm.Vals, `"confirm":"eu-prod"`) {
		t.Errorf("passing wizard = %+v", w)
	}
	if w.Envs[0].Size != "" {
		t.Errorf("unknown size shown as %q", w.Envs[0].Size)
	}
	// the old volumes stay on the source; a restore's source is gone
	if k := w.Confirm.Kept; len(k) != 1 || k[0] != "the backups, and the data on h3 until you remove it" {
		t.Errorf("kept = %q", k)
	}
	restore := strings.Replace(ok, `"OK":true`, `"OK":true,"Restore":true`, 1)
	if k := moveChecksView(&ui.MoveWizardView{}, plan(t, restore)).Confirm.Kept; len(k) != 1 || k[0] != "the backups" {
		t.Errorf("restore kept = %q", k)
	}
	// the service says OK but a line failed: no Apply
	bad := strings.Replace(ok, `"ok":true,"detail":"h4 free 12 GB"`, `"ok":false,"detail":"h4 free 1 GB, needs 9 GB"`, 1)
	if w := moveChecksView(&ui.MoveWizardView{}, plan(t, bad)); w.OK || w.Confirm.Action != "" {
		t.Errorf("a failing check still offers Apply: %+v", w)
	}
	// the service says no
	if w := moveChecksView(&ui.MoveWizardView{}, plan(t, strings.Replace(ok, `"OK":true`, `"OK":false`, 1))); w.OK {
		t.Errorf("OK=false offered Apply")
	}
	// only a volume with no backup failed: the typed confirm names it and acknowledges it
	empty := strings.Replace(strings.Replace(ok, `"OK":true`, `"OK":false,"Restore":true`, 1),
		`"ok":true,"detail":"h4 free 12 GB"`, `"ok":false,"overridable":true,"detail":"no backup for fresh; it starts empty"`, 1)
	if w := moveChecksView(&ui.MoveWizardView{}, plan(t, empty)); !w.OK || !w.Checks[1].Ack ||
		!strings.Contains(w.Confirm.Warning, "no backup for fresh; it starts empty") || !strings.Contains(w.Confirm.Vals, `"start_empty":"1"`) {
		t.Errorf("start-empty wizard = %+v", w)
	}
	if w := moveChecksView(&ui.MoveWizardView{}, plan(t, ok)); strings.Contains(w.Confirm.Vals, "start_empty") {
		t.Errorf("a plain move acknowledges empty volumes: %s", w.Confirm.Vals)
	}
}

func TestMoveProgressView(t *testing.T) {
	v := moveProgressView("eu-prod", service.PlacementMoveStatus{State: "running", From: "h3", To: "h4", Done: 1, All: 2,
		Envs: []service.MoveStatus{{Path: "acme/shop/prod", Active: true}}})
	if !strings.Contains(v.Poll, "/eu-prod/progress?poll=1") || v.Retry != "" || len(v.Envs) != 1 || v.Envs[0].Path != "acme/shop/prod" {
		t.Errorf("running = %+v", v)
	}
	v = moveProgressView("eu-prod", service.PlacementMoveStatus{State: "failed", Error: "disk full"})
	if v.Poll != "" || v.Retry != "/-/admin/placements/eu-prod/retry" {
		t.Errorf("failed = %+v", v)
	}
	if v := moveProgressView("eu-prod", service.PlacementMoveStatus{State: "done"}); v.Poll != "" || v.Retry != "" {
		t.Errorf("done = %+v", v)
	}
}

func TestGapView(t *testing.T) {
	n := gapView("node", "h4", []string{"ingress", "egress"})
	if n.AddNode != "/-/admin/nodes/add?name=h4&tags=ingress%2Cegress" || !strings.Contains(n.Label, "h4 (ingress, egress)") {
		t.Errorf("node gap = %+v", n)
	}
	s := gapView("secret", "db.password", nil)
	if s.Field != "secret.db.password" || s.Post != "/-/admin/config/gaps/secret" {
		t.Errorf("secret gap = %+v", s)
	}
}
