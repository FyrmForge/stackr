package env

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Settings is the accordion: the open row, its forms with the open row
// carried back, and the promote choice named after the rung below.
func TestTabs(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{
		Base:  "/e",
		Name:  "dev",
		From:  "promote",
		Below: "staging",
		Color: "teal",
		Open:  "deploys",
		Sums:  Sums{Deploys: "promoted from staging"},
	}).Render(context.Background(), &b)
	_ = Logs(c.LogHistoryView{Action: "/e/-/logs"}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{
		`<settings-find id="env-settings"`,
		`data-acc="deploys" open>`,
		`value="promote" checked`,
		"Promote from staging",
		`value="teal" checked`,
		`hx-post="/e/color"`,
		`name="open" value="deploys"`,
		`data-acc="delete"`,
		"promoted from staging</span>",
		`id="tab-logs"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
}

// The bottom rung has nothing to promote from; a reader gets no Delete row.
func TestSettingsBottomRungReader(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{Name: "dev", From: "branch"}).Render(context.Background(), &b)
	if s := b.String(); strings.Contains(s, `value="promote"`) || strings.Contains(s, `data-acc="delete"`) {
		t.Errorf("bottom rung offers promote or a reader sees Delete:\n%s", s)
	}
}

func TestSummaries(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{GeneralSummary("production", "rose", LockView{Show: true, Tiered: true, Locked: true}), "production · rose · locked (org tier)"},
		{GeneralSummary("dev", "", LockView{Show: true}), "dev · unlocked"},
		{GeneralSummary("pr-4", "teal", LockView{}), "pr-4 · teal"},
		{DeploysSummary("promote", "", false, "staging"), "promoted from staging"},
		{DeploysSummary("promote", "", false, ""), "promoted from the rung below"},
		{DeploysSummary("branch", "main", true, ""), "branch main · every push"},
		{DeploysSummary("branch", "", false, ""), "the default branch · by hand"},
		{ResourcesSummary([]c.SettingRowView{
			{Key: "cpu_limit", Effective: "2 cores", From: "org"},
			{Key: "mem_limit_mb", Value: "2048"},
			{Key: "protect", Effective: "false", From: "server"},
			{Key: "protect_user", Value: "admin"},
		}), "2 cores (from org) · 2048 MB · protect off (from server)"},
		{ResourcesSummary([]c.SettingRowView{{Key: "cpu_limit", Value: "0"}, {Key: "protect", Value: "true"}}), "no CPU limit · protect on"},
		{ResourcesSummary([]c.SettingRowView{{Key: "mem_limit_mb", Effective: "no limit", From: "server"}}), "no memory limit (from server)"},
		{VariablesSummary(12, 4), "12 set, 4 secret"},
		{VariablesSummary(2, 0), "2 set"},
		{VariablesSummary(0, 0), "none set"},
		{PlacementSummary("h4", "h4", ""), "runs on h4 · ingress h4 · own uplink"},
		{PlacementSummary("eu", "cluster default", "h3"), "runs on eu · ingress cluster default · exits via h3"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// The banner names the record to set and where DNS points now; a templ quirk once
// printed the current address twice.
func TestDNSBannerShowsRecordAndCurrent(t *testing.T) {
	var b strings.Builder
	_ = DNSBanner(DNSBannerView{URL: "/x", Hand: []DNSRow{{Host: "web.example.com", Records: "A 77.42.15.115", Points: "109.148.70.232"}}}).Render(context.Background(), &b)
	if !strings.Contains(b.String(), "set A 77.42.15.115. Now 109.148.70.232.") {
		t.Errorf("banner row = %s", b.String())
	}
}
