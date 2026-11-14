package slice

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func render(t *testing.T, v View, body templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := Drawer(v, body).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func full() View {
	return View{
		Node:          "t1",
		Name:          "main",
		Base:          "/o/s/e/-/slices/main",
		ProvisionFrom: "infra:${{ env.name }}:pg-db",
		Target:        "infra/staging/pg-db",
		TargetLink:    "/o/infra/staging?drawer=t9&tab=slices",
		DefaultAccess: "read",
		OnRemove:      "drop",
		Consumers: []Consumer{
			{Slug: "api", Link: "/o/s/e?drawer=t2&tab=access", Kind: "service", Access: "read", User: "api_user"},
		},
		Provisioned: true,
		DBName:      "main_db",
	}
}

// Overview: the target links to its instance's drawer, the consumers and
// the database; no form, no config key, no docker network.
func TestOverviewRenders(t *testing.T) {
	out := render(t, full(), Overview(full()))
	for _, w := range []string{
		`href="/o/infra/staging?drawer=t9&amp;tab=slices"`,
		"infra/staging/pg-db",
		`href="/o/s/e?drawer=t2&amp;tab=access"`,
		"api_user",
		"main_db",
		`?drawer=t1&amp;tab=settings`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q", w)
		}
	}
	for _, w := range []string{"<form", "provision_from", "slice_access", "stackr-operated"} {
		if strings.Contains(out, w) {
			t.Errorf("overview has %q", w)
		}
	}
}

// Settings: a row per thing the slice sets, each with its own Save and
// none posting on change; a value off its default marks its row and field.
func TestSettingsRows(t *testing.T) {
	v := full()
	v.Tab = "settings"
	out := render(t, v, Settings(v))
	for _, w := range []string{
		`hx-post="/o/s/e/-/slices/main/default-access"`,
		`hx-post="/o/s/e/-/slices/main/access"`,
		`hx-post="/o/s/e/-/slices/main/on-remove"`,
		`name="consumer" value="api"`,
		`data-acc="access" data-changed`,
		`data-acc="remove" data-changed`,
		`<option value="drop" selected>`,
		"/-/slices/main/delete",
		`word="main"`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q", w)
		}
	}
	if _, tab, _ := strings.Cut(out, `id="tab-settings"`); strings.Contains(tab, `hx-trigger="change"`) {
		t.Error("a slice setting still posts on change")
	}
	if n := strings.Count(out, "data-field data-changed"); n < 2 {
		t.Errorf("%d changed fields, want one in each changed row", n)
	}
	v.DefaultAccess, v.OnRemove = "", "keep"
	if out := render(t, v, Settings(v)); strings.Contains(out, `data-acc="access" data-changed`) || strings.Contains(out, `data-acc="remove" data-changed`) {
		t.Error("defaults are marked changed")
	}
}

// No target: the blocker in the error tone and what was written. A
// config-managed stack's default access is read-only.
func TestBlockerAndFileOwned(t *testing.T) {
	v := View{
		Name:          "main",
		Base:          "/o/s/e/-/slices/main",
		Blocker:       "shop:dev:pg does not admit acme:shop:prod:main",
		ProvisionFrom: "shop:dev:pg",
		DefaultAccess: "read",
		FileOwned:     true,
	}
	out := render(t, v, Overview(v)) + render(t, v, Settings(v))
	for _, w := range []string{"text-rw-danger", "does not admit", "Written as shop:dev:pg", "Set in the stack file."} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q", w)
		}
	}
	if strings.Contains(out, "/default-access") {
		t.Error("a config-managed stack's default access must not post")
	}
}

// A read-only slice drawer has no forms and no delete.
func TestReadOnlyNoWrites(t *testing.T) {
	v := full()
	v.ReadOnly, v.CanBranch = true, true
	out := render(t, v, Settings(v))
	for _, w := range []string{"<form", "Delete tile"} {
		if strings.Contains(out, w) {
			t.Errorf("read-only drawer has %q", w)
		}
	}
}

func TestConsumersSummary(t *testing.T) {
	for _, c := range []struct {
		in   []Consumer
		want string
	}{
		{[]Consumer{{Access: "write"}, {Access: "write"}}, "2 write"},
		{[]Consumer{{Access: "read"}}, "1 read"},
		{[]Consumer{{Access: "write"}, {Access: "read"}}, "1 write · 1 read"},
	} {
		if got := ConsumersSummary(c.in); got != c.want {
			t.Errorf("%v = %q, want %q", c.in, got, c.want)
		}
	}
}

// A bucket slice has the versions row, the restore row and the lazy size
// panel; any other slice has none of them.
func TestBucketRows(t *testing.T) {
	v := full()
	v.IsBucket, v.VersionDays, v.QuotaGB, v.CORS = true, 7, 2, "https://a.example.com"
	out := render(t, v, Settings(v)) + render(t, v, Overview(v))
	for _, w := range []string{`data-acc="bucket"`, `data-acc="restore"`, `name="version_days"`, `/o/s/e/-/slices/main/usage`, "old versions 7 d", "2 GB cap"} {
		if !strings.Contains(out, w) {
			t.Errorf("bucket drawer lacks %q", w)
		}
	}
	v.FileOwned = true
	if out := render(t, v, Settings(v)); strings.Contains(out, "/bucket\"") || !strings.Contains(out, "Set in the stack file.") {
		t.Error("a config-managed bucket must not post its settings")
	}
	v = full()
	if out := render(t, v, Settings(v)) + render(t, v, Overview(v)); strings.Contains(out, `data-acc="bucket"`) || strings.Contains(out, "/usage") {
		t.Error("a postgres slice shows bucket rows")
	}
	if SizeText(1536) != "1.5 KB" || SizeText(5<<30) != "5.0 GB" || SizeText(7) != "7 B" {
		t.Error("SizeText")
	}
}

// The Extensions row ticks only what the instance allows (plus anything the
// slice lists that it no longer allows), posts the whole list, and is a read
// when the stack file or the viewer's role owns it. The Overview lists what
// the database has, with CASCADE additions, and is a live region only when
// the drawer has a stream.
func TestExtensionsRowAndRegion(t *testing.T) {
	v := full()
	v.IsPostgres = true
	v.Ext = &ExtView{
		Allowed: []string{"vector", "postgis"},
		Listed:  []string{"postgis", "old_one"},
		Items: []ExtItem{
			{Name: "postgis", Installed: "3.4.2", Available: "3.5.0"},
			{Name: "postgis_topology", Installed: "3.4.2", AddedWith: "postgis"},
		},
	}
	set := render(t, v, Settings(v))
	for _, w := range []string{
		`data-acc="extensions"`, `hx-post="/o/s/e/-/slices/main/extensions"`, `name="extension" value="vector"`,
		`name="extension" value="postgis" checked`, `name="extension" value="old_one" checked`, `name="open"`,
	} {
		if !strings.Contains(set, w) {
			t.Errorf("settings lacks %q", w)
		}
	}
	if strings.Contains(set, "Remove") {
		t.Error("the Extensions row has a Remove button")
	}
	ov := render(t, v, Overview(v))
	for _, w := range []string{"slice-extensions", "3.4.2, 3.5.0 available", "added with postgis"} {
		if !strings.Contains(ov, w) {
			t.Errorf("overview lacks %q", w)
		}
	}
	if strings.Contains(ov, "sse-swap") {
		t.Errorf("the page is wired to a stream it has not:\n%s", ov)
	}
	v.Live = v.Base + "/live"
	frame := render(t, v, Overview(v))
	if !strings.Contains(frame, `sse-connect="/o/s/e/-/slices/main/live"`) || !strings.Contains(frame, `sse-swap="extensions"`) {
		t.Errorf("the drawer is not wired to its stream:\n%s", frame)
	}
	if got := render(t, v, LiveExtensions(v)); !strings.Contains(frame, got) {
		t.Errorf("the region differs between the page and the stream: %q", got)
	}
	v.FileOwned = true
	if out := render(t, v, Settings(v)); strings.Contains(out, "/extensions\"") || !strings.Contains(out, "Set in the stack file.") {
		t.Error("a config-managed slice must not post its extensions")
	}
	v.FileOwned, v.ReadOnly = false, true
	if out := render(t, v, Settings(v)); strings.Contains(out, "/extensions\"") {
		t.Error("a read-only viewer posts extensions")
	}
}

// A slice whose instance allows nothing says so; a failed read says so in
// the region, never an empty one; a slice with no extensions view draws no
// section.
func TestExtensionsEmptyAndUnread(t *testing.T) {
	v := full()
	v.IsPostgres, v.Ext = true, &ExtView{}
	if out := render(t, v, Settings(v)); !strings.Contains(out, "The instance allows none yet.") || strings.Contains(out, `name="extension"`) {
		t.Errorf("empty allowed list:\n%s", out)
	}
	if out := render(t, v, LiveExtensions(v)); !strings.Contains(out, "None yet.") {
		t.Errorf("empty region: %q", out)
	}
	v.Ext = &ExtView{Unread: "try again shortly"}
	if out := render(t, v, LiveExtensions(v)); !strings.Contains(out, "Could not read extensions: try again shortly") {
		t.Errorf("unread region: %q", out)
	}
	v.Ext = nil
	if out := render(t, v, Settings(v)) + render(t, v, Overview(v)); strings.Contains(out, "xtensions") {
		t.Error("a slice with no extensions view draws an Extensions section")
	}
}
