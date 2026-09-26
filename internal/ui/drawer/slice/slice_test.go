package slice

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Bindings show names, never values; detach only while a consumer holds it.
func TestOverviewRenders(t *testing.T) {
	v := View{
		Node:           "p1",
		Name:           "shop",
		DB:             "shop",
		OnRemove:       "keep",
		Instance:       "pg",
		InstanceNode:   "t9",
		InstanceDrawer: "/o/s/e/-/instances/pg?tab=slices",
		Logs:           true,
		Tab:            "bindings",
		Consumer:       true,
		Outputs:        []string{"DATABASE_URL"},
		Detach:         "/o/s/e/-/slices/p1/detach",
	}
	var b strings.Builder
	if err := Drawer(v, Overview(v)).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"DATABASE_URL", "/-/slices/p1/detach", "<confirm-dialog", "Open pg", "?drawer=t9&amp;tab=slices", "slice of pg", ">private<", "Overview", "tab=logs"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q", w)
		}
	}
}

func TestLogsOnlyWithInstance(t *testing.T) {
	if Tab("logs", false) != "bindings" || Tab("logs", true) != "logs" {
		t.Error("logs must need the instance in view")
	}
	var b strings.Builder
	if err := Logs(View{Instance: "pg"}, c.LogPaneView{StreamURL: "/x"}, true).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "pg's container logs") || !strings.Contains(b.String(), "<log-pane") {
		t.Error("logs tab lacks its note or pane")
	}
}
