package instance

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/FyrmForge/stackr/internal/ui/drawer/volume"
)

// Every tab draws inside v0's panel header with its marker; no tab draws
// the admin password (the view has none to draw).
func TestEveryTabRenders(t *testing.T) {
	v := View{
		Node:    "t1",
		Name:    "pg",
		Engine:  "postgres",
		Base:    "/o/s/e/-/instances/pg",
		Scope:   "stack",
		Running: true,
		Status:  "running",
	}
	for tab, tc := range map[string]struct {
		body templ.Component
		want []string
	}{
		"slices": {
			Overview(v, OverviewView{
				Endpoint:  "pg:5432",
				AdminUser: "stackr",
				Slices: []SliceRow{
					{ID: "p1", Name: "shop", DB: "shop", Drawer: "/o/s/e/-/slices/p1?tab=bindings", Public: true, UsedBy: "api"},
					{ID: "p2", Name: "old", OnRemove: "drop", Orphan: true},
				},
			}),
			[]string{"-/slices/p1", "public", "used by api", "consumer gone, drop pending", "the whole stack", "pg:5432"},
		},
		"logs": {
			Logs(v, LogsView{Running: true}),
			[]string{"<log-pane"},
		},
		"backups": {
			Backups(v, []volume.BackupsView{{ID: "v1", Name: "pg-data", Base: "/o/s/e/-/volumes/v1", Methods: []string{"dump", "volume"}}}),
			[]string{"Database dump", "/-/volumes/v1/backup", "No runs yet."},
		},
		"settings": {
			Settings(v),
			[]string{`name="scope_kind"`, `value="stack" selected`, "/-/instances/pg/delete", `word="pg"`},
		},
	} {
		var b strings.Builder
		if err := Drawer(v, tc.body).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		for _, w := range append(tc.want, "Overview", "/-/instances/pg/stop", ">stack-scoped<") {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s lacks %q", tab, w)
			}
		}
	}
}
