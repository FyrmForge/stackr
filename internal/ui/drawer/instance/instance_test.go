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
					{
						Slice:    "shop-db",
						Link:     "/o/s/e?drawer=t2&tab=overview",
						Name:     "shop_db",
						Bindings: 1,
						Public:   true,
					},
					{
						Slice:    "shop-db",
						Where:    "shop/prod",
						Link:     "/o/shop/prod?drawer=t3&tab=overview",
						Name:     "shop_prod",
						Bindings: 2,
					},
				},
			}),
			[]string{
				"Slices on this instance",
				`href="/o/s/e?drawer=t2&amp;tab=overview"`,
				"in shop/prod",
				"1 binding<",
				"2 bindings",
				"public",
				"pg:5432",
			},
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
			Settings(v, SettingsView{
				Allow:    []string{"o:shop:*", "o:web:prod:api"},
				Add:      "o:",
				CanAllow: true,
				Pairs: []Pair{
					{
						From: "staging",
						To:   "prod",
					},
				},
				Envs:   []string{"dev", "prod"},
				Errors: map[string]string{"allow": "o:x:y: four segments or a trailing *"},
			}),
			[]string{
				"o:shop:*",
				`hx-post="/o/s/e/-/instances/pg/allow"`,
				`name="allow" value="o:web:prod:api"`,
				`name="add" value="o:"`,
				"four segments or a trailing *",
				"staging",
				`hx-post="/o/s/e/-/instances/pg/env-pairs"`,
				`<option value="prod">prod</option>`,
				"/-/instances/pg/delete",
				`word="pg"`,
			},
		},
	} {
		var b strings.Builder
		if err := Drawer(v, tc.body).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		for _, w := range append(tc.want, "Overview", "/-/instances/pg/stop") {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s lacks %q", tab, w)
			}
		}
	}
}

// A member who is not an owner sees the allow list but no form to change it.
func TestAllowIsAnOwners(t *testing.T) {
	v := View{
		Name: "pg",
		Base: "/o/s/e/-/instances/pg",
	}
	var b strings.Builder
	s := SettingsView{Allow: []string{"o:shop:*"}}
	if err := Settings(v, s).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "/allow") || !strings.Contains(b.String(), "Only an owner") {
		t.Error("a non-owner must not get the allow forms")
	}
}
