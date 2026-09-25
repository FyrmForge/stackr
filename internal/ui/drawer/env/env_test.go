package env

import (
	"context"
	"strings"
	"testing"
)

func TestTabs(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{
		Base:  "/e",
		Name:  "dev",
		From:  "promote",
		Color: "teal",
	}).Render(context.Background(), &b)
	_ = Order(OrderView{
		Action: "/e/order",
		Rungs: []Rung{
			{ID: "a", Name: "dev", Up: []string{"b", "a"}},
			{ID: "b", Name: "prod", Down: []string{"b", "a"}},
		},
	}).Render(context.Background(), &b)
	_ = Logs().Render(context.Background(), &b)
	got := b.String()
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
	if strings.Count(got, "<button type=\"submit\" class=\"btn\">") != 2 {
		t.Error("the ends of the ladder offer a move off it")
	}
}
