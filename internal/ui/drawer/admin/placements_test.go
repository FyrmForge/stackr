package admin

import (
	"context"
	"io"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func render(t *testing.T, comp interface {
	Render(ctx context.Context, w io.Writer) error
}) string {
	t.Helper()
	var b strings.Builder
	if err := comp.Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Step 2 lists every check with its detail; a failure leaves Apply disabled
// and offers Re-check, no confirm dialog.
func TestMoveWizardChecks(t *testing.T) {
	v := MoveWizardView{
		Placement: "eu-prod", From: "h3", To: "h4", Step: 2, Check: "/x/move", Cancel: "/x",
		Checks: []CheckRow{{Name: "target online", OK: true}, {Name: "disk", Detail: "h4 free 1 GB, needs 9 GB"}},
		Envs:   []MoveEnvRow{{Path: "acme/shop/prod", Volumes: 1, Downtime: "about 20 s"}},
	}
	out := render(t, MoveWizard(v))
	for _, want := range []string{"target online", "h4 free 1 GB, needs 9 GB", "Re-check", "disabled", "acme/shop/prod", "about 20 s", "Failed:"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "<confirm-dialog") {
		t.Errorf("a failing wizard offers the confirm\n%s", out)
	}
	v.OK, v.Checks[1].OK, v.Checks[1].Detail = true, true, ""
	v.Confirm = c.ConfirmView{Button: "Move", Title: "Move eu-prod to h4?", Word: "eu-prod", Kept: []string{"data"}, Action: "/x/move", Target: "#drawer"}
	out = render(t, MoveWizard(v))
	if !strings.Contains(out, "<confirm-dialog") || !strings.Contains(out, `word="eu-prod"`) || strings.Contains(out, "disabled") {
		t.Errorf("passing wizard lacks the confirm\n%s", out)
	}
}

// Progress polls only while the move runs and offers Retry once it failed.
func TestMoveProgress(t *testing.T) {
	out := render(t, MoveProgress(MoveProgressView{Placement: "eu", State: "running", From: "h3", To: "h4", Done: 1, All: 2, Poll: "/p?poll=1", Back: "/b"}))
	if !strings.Contains(out, `hx-trigger="every 3s"`) || !strings.Contains(out, "1 of 2 envs moved.") || strings.Contains(out, "Retry") {
		t.Errorf("running\n%s", out)
	}
	out = render(t, MoveProgress(MoveProgressView{Placement: "eu", State: "failed", Error: "disk full", Retry: "/r", Back: "/b"}))
	if strings.Contains(out, "every 3s") || !strings.Contains(out, `hx-post="/r"`) || !strings.Contains(out, "disk full") {
		t.Errorf("failed\n%s", out)
	}
}

func TestPlacementsList(t *testing.T) {
	out := render(t, Placements(PlacementsView{
		Rows: []PlacementRow{{Name: "eu-prod", Node: "h4", Tags: []string{"ingress"}, Envs: 2, Open: "all orgs", State: "online", Tone: "success", Move: "/m"}},
		Add:  PlacementFormView{Action: "/-/admin/placements", All: true, Nodes: []Option{{Value: "h4", Label: "h4"}}},
	}))
	for _, want := range []string{"eu-prod", "on h4", "2 envs", "Repoint", "Add placement", `value="h4"`, "None yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

func TestAddNodePrefill(t *testing.T) {
	out := render(t, AddNode(AddNodeView{Step: 1, Name: "h4", Tags: []string{"ingress", "egress", "storage"}, Picked: []string{"storage"}, Create: "/c", Cancel: "/x"}))
	if !strings.Contains(out, `value="h4"`) || !strings.Contains(out, `value="storage" checked`) || strings.Contains(out, "Open to") {
		t.Errorf("prefill\n%s", out)
	}
}
