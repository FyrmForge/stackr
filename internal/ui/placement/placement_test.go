package placement_test

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	ui "github.com/FyrmForge/stackr/internal/ui/placement"
)

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// The pin change dialog asks to confirm only when every check passed; a
// failing check lists itself and offers no Confirm.
func TestMoveDialog(t *testing.T) {
	v := ui.MoveView{
		Action: "/acme/shop/prod/-/placement/move", Env: "prod", From: "eu-prod", To: "eu-db",
		Envs:     []ui.MoveLine{{Path: "acme/shop/prod", Detail: "2 volumes"}},
		Checks:   []ui.CheckLine{{Name: "tags", OK: true}, {Name: "disk", OK: false, Detail: "needs 9 GB"}},
		Downtime: "20 s", Vals: `{"placement":"eu-db","confirm":"prod"}`,
	}
	body := render(t, ui.Move(v))
	for _, w := range []string{"cannot move to eu-db yet", "needs 9 GB", "Fail", "Pass", "acme/shop/prod"} {
		if !strings.Contains(body, w) {
			t.Errorf("blocked dialog lacks %q", w)
		}
	}
	if strings.Contains(body, "confirm-dialog") {
		t.Error("a failing check still offers Confirm")
	}
	v.OK, v.Checks[1].OK = true, true
	body = render(t, ui.Move(v))
	for _, w := range []string{"confirm-dialog", "Move prod to eu-db?", "Stops for 20 s", "/acme/shop/prod/-/placement/move"} {
		if !strings.Contains(body, w) {
			t.Errorf("confirm dialog lacks %q", w)
		}
	}
}

// The org list names placements, never nodes.
func TestOffers(t *testing.T) {
	body := render(t, ui.Placement(ui.View{Scope: "org", RunsOn: true, Offers: []ui.Offer{
		{Name: "eu-prod", Tags: "ingress", Public: true, Usable: true},
		{Name: "eu-db", Tags: "storage", Why: "offline"},
	}}))
	for _, w := range []string{"eu-prod", "ingress", "eu-db", "offline", "Usable"} {
		if !strings.Contains(body, w) {
			t.Errorf("list lacks %q", w)
		}
	}
}
