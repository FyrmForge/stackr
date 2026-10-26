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
			Backups(v, []volume.BackupsView{{ID: "v1", Name: "pg-data", Base: "/o/s/e/-/volumes/v1", Edit: true, Methods: []string{"dump", "volume"}}}),
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

// The Settings rows closed: the allow patterns or the default, the pairs.
func TestSettingsSummaries(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{AllowSummary(nil), "this environment only"},
		{AllowSummary([]string{"acme:shop:*", "acme:web:prod:api"}), "acme:shop:*, acme:web:prod:api"},
		{PairsSummary(nil), "none"},
		{PairsSummary([]Pair{{"staging", "dev"}, {"prod", "prod"}}), "staging → dev, prod → prod"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// Allow and Env pairs are changed once they hold an entry, each entry a
// changed field; empty, nothing is.
func TestSettingsRowsChanged(t *testing.T) {
	s := SettingsView{Allow: []string{"acme:*"}, Pairs: []Pair{{"staging", "dev"}}}
	for _, r := range settingsRows(View{Base: "/i"}, s) {
		if want := r.Key != "delete"; r.Changed != want {
			t.Errorf("%s changed = %v", r.Key, r.Changed)
		}
		var b strings.Builder
		_ = r.Body.Render(context.Background(), &b)
		if r.Changed && !strings.Contains(b.String(), "data-field data-changed") {
			t.Errorf("%s is changed but no field in it is", r.Key)
		}
		if strings.Contains(b.String(), "provision_from") {
			t.Errorf("%s names a config key", r.Key)
		}
	}
	for _, r := range settingsRows(View{}, SettingsView{}) {
		if r.Changed {
			t.Errorf("empty %s is changed", r.Key)
		}
	}
}
