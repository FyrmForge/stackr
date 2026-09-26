package slice

import (
	"context"
	"strings"
	"testing"
)

func render(t *testing.T, v View) string {
	t.Helper()
	var b strings.Builder
	if err := Drawer(v, Overview(v)).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The target links to its instance's drawer; each consumer's access is a
// select that posts; default access and on remove post on change.
func TestOverviewRenders(t *testing.T) {
	v := View{
		Node:          "t1",
		Name:          "main",
		Base:          "/o/s/e/-/slices/main",
		ProvisionFrom: "infra:${{ env.name }}:pg-db",
		Target:        "infra/staging/pg-db",
		TargetLink:    "/o/infra/staging?drawer=t9&tab=slices",
		DefaultAccess: "write",
		OnRemove:      "drop",
		Consumers: []Consumer{
			{
				Slug:   "api",
				Link:   "/o/s/e?drawer=t2&tab=access",
				Kind:   "service",
				Access: "read",
				User:   "api_user",
			},
		},
		Provisioned: true,
		DBName:      "main_db",
		Network:     "stackr-managed-m1",
	}
	out := render(t, v)
	for _, w := range []string{
		`href="/o/infra/staging?drawer=t9&amp;tab=slices"`,
		"infra/staging/pg-db",
		`hx-post="/o/s/e/-/slices/main/default-access"`,
		`hx-post="/o/s/e/-/slices/main/access"`,
		`name="consumer" value="api"`,
		`<option value="read" selected>`,
		`href="/o/s/e?drawer=t2&amp;tab=access"`,
		"api_user",
		`<option value="drop" selected>`,
		"main_db",
		"stackr-managed-m1",
		"/-/slices/main/delete",
		`word="main"`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q", w)
		}
	}
}

// No target: the blocker in the error tone. A config-managed stack's
// default access is read-only.
func TestBlockerAndFileOwned(t *testing.T) {
	out := render(t, View{
		Name:          "main",
		Base:          "/o/s/e/-/slices/main",
		Blocker:       "shop:dev:pg does not admit acme:shop:prod:main",
		DefaultAccess: "read",
		FileOwned:     true,
	})
	for _, w := range []string{"text-rw-danger", "does not admit", "Set in the stack file."} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q", w)
		}
	}
	if strings.Contains(out, "/default-access") {
		t.Error("a config-managed stack's default access must not post")
	}
}
