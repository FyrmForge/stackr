package stack

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/access"
	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestHostAccess(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{Name: "none"}).Render(context.Background(), &b)
	if strings.Contains(b.String(), `id="host-access"`) || !strings.Contains(b.String(), "No tile has host access.") {
		t.Error("a stack with no host access lists grants")
	}
	b.Reset()
	_ = Settings(SettingsView{Name: "mon", Host: HostView{
		Tiles: access.Group([]string{"web host:/a:/b", "web privileged"}, []string{"web device:/dev/x"}),
		Approve: &access.ApproveView{
			ID:      "a",
			Action:  "/approve",
			Name:    "mon",
			Pending: []string{"web device:/dev/x"},
			Tiles:   access.Group(nil, []string{"web device:/dev/x"}),
		},
	}}).Render(context.Background(), &b)
	for _, w := range []string{
		"Host access", "Host folder", "Privileged", "Device", "waiting",
		`hx-post="/approve"`, `name="grant" value="web device:/dev/x" checked`,
		"2 grants on web · 1 waiting",
	} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("no %s in\n%s", w, b.String())
		}
	}
	if strings.Contains(b.String(), `name="confirm"`) {
		t.Error("a device asks for the stack name")
	}
}

func TestSettingsRows(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{
		Base:       "/o/s/-/drawer",
		Open:       "order",
		Name:       "shop",
		Connectors: []Option{{Value: "k1", Label: "gh"}},
		Connector:  "k1",
		Repo:       "acme/infra",
		Order: OrderView{Action: "/o/s/-/drawer/order", Rungs: []Rung{
			{ID: "a", Name: "dev", Detail: "branch main", Down: []string{"b", "a"}},
			{ID: "b", Name: "prod", Detail: "from dev", Up: []string{"b", "a"}},
		}},
		Delete: c.ConfirmView{Word: "shop", Kept: []string{"x"}, Action: "/o/s/-/drawer/delete"},
	}).Render(context.Background(), &b)
	_ = c.Releases(c.ReleasesView{Rows: []c.ReleaseRow{{Number: "3", By: "dev", Message: "fix <b>x</b>"}}}).Render(context.Background(), &b)
	_ = Settings(SettingsView{Name: "ro"}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{
		"<settings-find",
		`data-acc="general"`, `data-acc="order"`, `data-acc="config"`, `data-acc="resources"`,
		`data-acc="variables"`, `data-acc="placement"`, `data-acc="host"`, `data-acc="delete"`,
		`data-acc="order" open`,
		"dev, prod",
		`hx-post="/o/s/-/drawer/order"`,
		`aria-label="Move dev down"`,
		`hx-post="/o/s/-/drawer/config"`,
		`<option value="k1" selected>`,
		`value="acme/infra"`,
		"<confirm-dialog",
		"#3",
		`title="fix &lt;b&gt;x&lt;/b&gt;"`,
		`<fieldset disabled`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	// read-only: no move buttons, no delete row
	ro := got[strings.LastIndex(got, `<div id="tab-settings">`):]
	if strings.Contains(ro, `data-acc="delete"`) || strings.Contains(ro, "/order") {
		t.Errorf("read-only settings offer delete or a move:\n%s", ro)
	}
}

// After a move the focus goes back to the moved rung's button in the same
// direction, or the other one once the rung reached the end.
func TestOrderFocus(t *testing.T) {
	top := Rung{ID: "a", Up: nil, Down: []string{"b", "a"}}
	mid := Rung{ID: "b", Up: []string{"b", "a"}, Down: []string{"a", "b"}}
	for _, tc := range []struct {
		moved string
		r     Rung
		dir   string
		want  bool
	}{
		{"b:up", mid, "up", true},
		{"b:up", mid, "down", false},
		{"a:up", top, "up", false}, // reached the top: no up button left
		{"a:up", top, "down", true},
		{"a:down", mid, "down", false}, // another rung
		{"", mid, "up", false},
	} {
		if got := (OrderView{Moved: tc.moved}).Focus(tc.r, tc.dir); got != tc.want {
			t.Errorf("moved %q, rung %s %s: %v, want %v", tc.moved, tc.r.ID, tc.dir, got, tc.want)
		}
	}
}

func TestSummaries(t *testing.T) {
	for _, tc := range [][2]string{
		{GeneralSummary("home", "home"), "home"},
		{GeneralSummary("Home App", "home-app"), "Home App · home-app"},
		{OrderSummary(nil), "no environments"},
		{OrderSummary([]Rung{{Name: "dev"}, {Name: "staging"}, {Name: "production"}}), "dev, staging, production"},
		{ConfigSummary("", "", ""), "managed from the UI"},
		{ConfigSummary("https://github.com/FyrmForge/stackr-test.git", "", ""), "FyrmForge/stackr-test · stackr-compose.yml"},
		{ConfigSummary("acme/infra", "main", "deploy/stack.yml"), "acme/infra · main · deploy/stack.yml"},
		{ResourcesSummary([]c.SettingRowView{{Key: "cpu_limit"}}), "inherits org"},
		{ResourcesSummary([]c.SettingRowView{{Value: "2"}, {Secret: true, Effective: "set"}, {}}), "2 set here"},
		{PlacementSummary("", "", ""), "inherits org (default)"},
		{PlacementSummary("", "h4", ""), "runs on h4 (from org)"},
		{PlacementSummary("h3", "h3", "h4"), "runs on h3 · ingress h4"},
		{DeleteSummary(0), "No envs"},
		{DeleteSummary(2), "Holds 2 envs"},
		{HostSummary(nil), "none"},
		{HostSummary(access.Group([]string{"web privileged"}, []string{"api device:/dev/x"})), "1 grant on web · 1 waiting"},
	} {
		if tc[0] != tc[1] {
			t.Errorf("got %q, want %q", tc[0], tc[1])
		}
	}
}
