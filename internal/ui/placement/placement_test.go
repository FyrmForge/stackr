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

// An idle move's dialog asks for no typed word.
func TestMoveDialogIdleTypesNothing(t *testing.T) {
	v := ui.MoveView{Action: "/m", Env: "prod", From: "a", To: "b", OK: true, Idle: true, Vals: `{}`}
	if body := render(t, ui.Move(v)); strings.Contains(body, "to confirm") {
		t.Error("an idle move asks for a typed word")
	}
	v.Idle = false
	if body := render(t, ui.Move(v)); !strings.Contains(body, "to confirm") {
		t.Error("a busy move lost its typed word")
	}
}

// The DNS list gives each hand-managed record a copy button, lists managed
// hosts apart with a ticked repoint, and offers Check again.
func TestDNSList(t *testing.T) {
	body := render(t, ui.DNS(ui.DNSView{
		Hand:        []ui.DNSHost{{Host: "shop.example.com", Rows: []ui.DNSRow{{Type: "A", Name: "shop.example.com", Value: "198.51.100.2"}}, Now: "198.51.100.1"}},
		Managed:     []ui.DNSHost{{Host: "app.acme.dev", Old: "198.51.100.1", New: "198.51.100.2", OK: true}},
		Connectors:  "Namecheap",
		CheckAction: "/acme/-/placement/dnscheck", CheckVals: `{}`,
	}))
	for _, w := range []string{`data-copy="198.51.100.2"`, "Repointed for you", "198.51.100.1 to 198.51.100.2", "Let Namecheap repoint these", `name="repoint"`, "checked", "Check again", "Matches"} {
		if !strings.Contains(body, w) {
			t.Errorf("dns list lacks %q", w)
		}
	}
}

// Hosts onto a node with no public IP are listed apart with the note: no
// repoint, no record rows, no repoint tick.
func TestDNSListHeldHosts(t *testing.T) {
	body := render(t, ui.DNS(ui.DNSView{
		Held: []ui.DNSHost{{Host: "shop.example.com", Note: "Node h4 has no public IP address; records are left as they are"}},
	}))
	for _, w := range []string{"Left as they are", "shop.example.com", "Node h4 has no public IP address"} {
		if !strings.Contains(body, w) {
			t.Errorf("dns list lacks %q", w)
		}
	}
	for _, w := range []string{"Repointed for you", "Point these hosts here", `name="repoint"`} {
		if strings.Contains(body, w) {
			t.Errorf("dns list has %q for a held host", w)
		}
	}
}
