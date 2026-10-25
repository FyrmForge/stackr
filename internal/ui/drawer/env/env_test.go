package env

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestTabs(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{
		Base:  "/e",
		Name:  "dev",
		From:  "promote",
		Color: "teal",
	}).Render(context.Background(), &b)
	var ob strings.Builder
	_ = Order(OrderView{
		Action: "/e/order",
		Rungs: []Rung{
			{ID: "a", Name: "dev", Up: []string{"b", "a"}},
			{ID: "b", Name: "prod", Down: []string{"b", "a"}},
		},
	}).Render(context.Background(), &ob)
	_ = Logs(c.LogHistoryView{Action: "/e/-/logs"}).Render(context.Background(), &b)
	got := b.String() + ob.String()
	for _, w := range []string{
		`value="promote" checked`,
		`value="teal" checked`,
		`hx-post="/e/color"`,
		`hx-post="/e/order"`,
		`name="ids" value="b"`,
		`id="tab-logs"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	if strings.Count(ob.String(), `<button type="submit"`) != 2 {
		t.Error("the ends of the ladder offer a move off it")
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
