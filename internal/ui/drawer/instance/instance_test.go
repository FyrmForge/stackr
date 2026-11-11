package instance

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	c "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/drawer/volume"
)

// A failed read shows one line in its region, never nothing; the job is a
// live region the stream swaps.
func TestUnreadRegionsAndLiveJob(t *testing.T) {
	render := func(c templ.Component) string {
		var b strings.Builder
		if err := c.Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	v := View{Base: "/o/s/e/-/instances/pg", Live: "/live"}
	o := OverviewView{Unread: map[string]string{"pooler": "try again shortly", "members": "x", "sync": "y", "failover": "z"}}
	for want, got := range map[string]string{
		"read the pooler: try again shortly": render(LivePooler(v, o)),
		"read members: x":                    render(LiveMembers(v, o)),
		"read sync: y":                       render(LiveSync(v, o)),
		"read failover: z":                   render(LiveFailover(v, o)),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
	j := c.JobStatusView{Kind: "deploy", State: "running"}
	if got := render(LiveJob(v, OverviewView{Job: &j})); !strings.Contains(got, "Last job") {
		t.Errorf("job region lacks the job: %q", got)
	}
	if got := render(Overview(v, OverviewView{Job: &j})); !strings.Contains(got, `sse-swap="job"`) {
		t.Errorf("job is not a live region:\n%s", got)
	}
}

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
			Backups(v, PITRView{}, []volume.BackupsView{{ID: "v1", Name: "pg-data", Base: "/o/s/e/-/volumes/v1", Edit: true, Methods: []string{"dump", "volume"}}}),
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
		if want := r.Key != "delete" && r.Key != "general"; r.Changed != want { // a name is never "changed"
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

// The Overview offers a version change with the majors it can reach, a
// rollback that asks the name typed while the old data is kept, and neither to
// a read-only viewer.
func TestMajorChangeAndRollbackRender(t *testing.T) {
	v := View{Node: "t1", Name: "pg", Engine: "postgres", Base: "/o/s/e/-/instances/pg", Running: true}
	o := OverviewView{Version: &VersionView{Image: "postgres:17.5", Majors: []int{16, 18}, Check: "Last check for postgres 18: ok", RollbackFrom: 16, RollbackUntil: "2026-11-16 10:00"}}
	var b strings.Builder
	if err := Overview(v, o).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`hx-post="/o/s/e/-/instances/pg/major"`, `<option value="18">`, `value="check"`, "Change version",
		"Last check for postgres 18: ok", `hx-post="/o/s/e/-/instances/pg/rollback-upgrade"`, "Type pg to roll back", "kept until 2026-11-16 10:00"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	v.ReadOnly = true
	b.Reset()
	if err := Overview(v, o).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "/major") {
		t.Error("a read-only viewer is offered a version change")
	}
}

// With a live stream the overview opens it and wraps the four regions the
// stream swaps; without one it draws the same parts bare. A region renders
// the same markup in the page and in the stream.
func TestOverviewLiveRegions(t *testing.T) {
	o := OverviewView{
		Endpoint: "pg:5432",
		Members:  []MemberRow{{ID: "m1", Node: "w1", Role: "replica", State: "streaming", Tone: "success", CanFence: true}},
		Failover: &FailoverView{Primary: "ctl", Switch: []string{"w1"}},
	}
	render := func(c templ.Component) string {
		var b strings.Builder
		if err := c.Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	v := View{Engine: "postgres", Base: "/o/s/e/-/instances/pg"}
	bare := render(Overview(v, o))
	if strings.Contains(bare, "sse-connect") || strings.Contains(bare, "sse-swap=\"members\"") {
		t.Errorf("no stream, yet the page is wired to one:\n%s", bare)
	}
	v.Live = v.Base + "/live"
	live := render(Overview(v, o))
	for _, want := range []string{`sse-connect="/o/s/e/-/instances/pg/live"`, `sse-swap="pooler"`, `sse-swap="members"`, `sse-swap="failover"`, `sse-swap="sync"`, "Switch over"} {
		if !strings.Contains(live, want) {
			t.Errorf("live overview lacks %q", want)
		}
	}
	if m := render(LiveMembers(v, o)); !strings.Contains(live, m) || !strings.Contains(m, "streaming") {
		t.Errorf("the members region differs between the page and the stream: %q", m)
	}
}

// The live forms are preserved across a swap under an id that names what
// they offer: the same options keep the form (and what is typed or picked),
// new options give a new id, so the form is replaced and never stale.
func TestLiveFormsArePreserved(t *testing.T) {
	render := func(c templ.Component) string {
		var b strings.Builder
		if err := c.Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	v := View{Base: "/o/s/e/-/instances/pg", Live: "/live"}
	id := func(html, prefix string) string {
		i := strings.Index(html, `id="`+prefix+"-")
		if i < 0 {
			t.Fatalf("no %s form with an id in:\n%s", prefix, html)
		}
		rest, _, _ := strings.Cut(html[i+4:], `"`)
		return rest
	}
	fo := func(switchTo ...string) string {
		return render(LiveFailover(v, OverviewView{Members: []MemberRow{{}}, Failover: &FailoverView{Primary: "ctl", Switch: switchTo, Promote: []string{"w1"}}}))
	}
	a, b, c := fo("w1"), fo("w1"), fo("w1", "w2")
	if !strings.Contains(a, "hx-preserve") || id(a, "switch-form") != id(b, "switch-form") || id(a, "switch-form") == id(c, "switch-form") {
		t.Errorf("switch form ids: %q %q %q", id(a, "switch-form"), id(b, "switch-form"), id(c, "switch-form"))
	}
	if id(a, "promote-form") != id(c, "promote-form") {
		t.Error("the promote form changed with the switch options")
	}
	sy := func(limit int, status string) string {
		return render(LiveSync(v, OverviewView{Sync: &SyncView{LimitMS: limit, Status: status, Standbys: []SyncStandby{{ID: "m1", Mode: "auto"}}}}))
	}
	if id(sy(50, "protected"), "sync-form") != id(sy(50, "unprotected"), "sync-form") || id(sy(50, "x"), "sync-form") == id(sy(80, "x"), "sync-form") {
		t.Error("the sync form id must ignore the status line and follow the saved values")
	}
	add := func(nodes ...string) string {
		return render(LiveMembers(v, OverviewView{AddNodes: nodes}))
	}
	if one, again := add("w1"), add("w1"); id(one, "standby-add") != id(again, "standby-add") || id(one, "standby-add") == id(add("w1", "w2"), "standby-add") {
		t.Error("the add-standby form id must follow the candidate nodes")
	}
}
