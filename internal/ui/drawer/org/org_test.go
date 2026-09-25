package org

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func html(t *testing.T, x templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := x.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTabs(t *testing.T) {
	for name, tc := range map[string]struct {
		x        templ.Component
		want, no []string
	}{
		"owner settings": {
			Settings(SettingsView{
				Name:   "acme",
				Rename: "/acme/-/drawer/rename",
				Delete: c.ConfirmView{
					Button: "Delete org",
					Word:   "acme",
					Kept:   []string{"x"},
					Action: "/acme/-/drawer/delete",
				},
			}),
			[]string{`hx-post="/acme/-/drawer/rename"`, "<confirm-dialog", `hx-post="/acme/-/drawer/delete"`},
			nil,
		},
		"viewer settings": {
			Settings(SettingsView{Name: "acme"}),
			[]string{`value="acme"`, "disabled"},
			[]string{"<confirm-dialog"},
		},
		"members": {
			Members(MembersView{
				Base:    "/o",
				Manage:  true,
				Roles:   []string{"owner"},
				Members: []MemberRow{{UserID: "u1", Email: "a@b.c", Role: "owner"}},
				Invites: []InviteRow{{Email: "new@b.c", Role: "member"}},
			}),
			[]string{
				"a@b.c",
				`hx-post="/o/members/u1/role"`,
				`<option value="owner" selected>`,
				"new@b.c",
				`hx-post="/o/invite"`,
			},
			nil,
		},
		"members read": {
			Members(MembersView{Members: []MemberRow{{Email: "a@b.c", Role: "viewer"}}}),
			[]string{"viewer"},
			[]string{"invite", "<select"},
		},
		"keys": {
			Keys(KeysView{
				Base: "/o",
				Keys: []KeyRow{
					{ID: "k1", Name: "laptop"},
				},
			}),
			[]string{"laptop", `hx-post="/o/keys/k1/revoke"`, `hx-post="/o/keys"`},
			nil,
		},
		"backups": {
			Backups(c.DestsView{
				Scope: "organization",
				Add:   "/o/backups",
				Rows: []c.DestView{
					{Name: "local", Note: "install-wide"},
					{Name: "s3", Remove: c.ConfirmView{Button: "Remove", Action: "/o/backups/d2/delete"}},
				},
			}),
			[]string{"install-wide", `hx-post="/o/backups/d2/delete"`, `name="secret_key"`},
			[]string{"/o/backups/d1/delete"},
		},
	} {
		got := html(t, tc.x)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: no %s in\n%s", name, w, got)
			}
		}
		for _, n := range tc.no {
			if strings.Contains(got, n) {
				t.Errorf("%s: %s in\n%s", name, n, got)
			}
		}
	}
}
