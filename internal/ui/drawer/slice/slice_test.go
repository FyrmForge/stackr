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
	for _, w := range []string{"<form", "provision_from", "slice_access", "stackr-managed"} {
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
	v.ReadOnly = true
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
