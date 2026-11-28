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
				Invites: []InviteRow{{Email: "new@b.c", Role: "member", Link: "https://x/invite/i1"}},
			}),
			[]string{
				"a@b.c",
				`hx-post="/o/members/u1/role"`,
				"Remove a@b.c?",
				`data-copy="https://x/invite/i1"`,
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
				Base:   "/o",
				Minted: "tok123",
				Keys: []KeyRow{
					{ID: "k1", Name: "laptop"},
				},
			}),
			[]string{"laptop", "Revoke laptop?", `hx-post="/o/keys/k1/revoke"`, `hx-post="/o/keys"`, `data-copy="tok123"`, `hx-history="false"`, "not shown again"},
			nil,
		},
		"keys role and stack": {
			Keys(KeysView{
				Base:   "/o",
				Roles:  []string{"viewer", "member"},
				Stacks: []StackOpt{{Slug: "shop", Name: "Shop"}},
				Keys:   []KeyRow{{ID: "k1", Name: "ci", Role: "viewer", Stack: "shop"}},
			}),
			[]string{`name="level"`, `name="stack"`, "Whole org", `value="shop"`, "Sees everything, changes nothing.", ">viewer<", ">shop<"},
			[]string{`value="owner"`},
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

// The org's defaults row is changed when it sets a knob, with that knob's
// field marked; the rest of General holds no inherited value to differ from.
func TestGeneralRowsChanged(t *testing.T) {
	v := SettingsView{Name: "acme", Cascade: c.SettingsFormView{ID: "d", Rows: []c.SettingRowView{{Key: "cpu_limit", Label: "CPU", Type: "float", Value: "1", Changed: true}}}}
	for _, r := range generalRows(v) {
		if r.Changed != (r.Key == "defaults") {
			t.Errorf("%s changed = %v", r.Key, r.Changed)
		}
		if r.Key == "defaults" && !strings.Contains(html(t, r.Body), "data-changed") {
			t.Error("the defaults row is changed but no field in it is")
		}
	}
}

// The Config tab: unbound it offers the guided dialog; bound it shows the
// facts with the form folded; a waiting plan comes before the facts.
func TestConfigTabStates(t *testing.T) {
	unbound := html(t, Config(ConfigView{Base: "/acme/-/drawer", Picker: "/p", Export: "/x"}))
	for _, w := range []string{"Bind a repository", `hx-get="/p"`, ">Export</summary>"} {
		if !strings.Contains(unbound, w) {
			t.Errorf("unbound lacks %q:\n%s", w, unbound)
		}
	}
	if strings.Contains(unbound, `name="repo"`) || strings.Contains(unbound, "Last plan") {
		t.Errorf("unbound draws the form or facts:\n%s", unbound)
	}

	bound := ConfigView{Base: "/acme/-/drawer", Export: "/x", Repo: "acme/org", Branch: "master", Auto: true}
	b := html(t, Config(bound))
	for _, w := range []string{">acme/org<", ">master", "stackr-org.yml", ">On", ">Change</summary>", ">Export</summary>", `name="repo"`} {
		if !strings.Contains(b, w) {
			t.Errorf("bound lacks %q:\n%s", w, b)
		}
	}
	if strings.Contains(b, "Bind a repository") || strings.Index(b, "<details") > strings.Index(b, `name="repo"`) {
		t.Errorf("bound: the form is not folded:\n%s", b)
	}

	bound.Latest = &PlanBox{Row: c.PlanRow{Status: "pending", Summary: "1 to add"}}
	p := html(t, Config(bound))
	if l, f := strings.Index(p, "Latest plan"), strings.Index(p, ">Repository</dt>"); l < 0 || f < 0 || l > f {
		t.Errorf("a waiting plan is not first (plan %d, facts %d):\n%s", l, f, p)
	}
	bound.Latest.Row.Status = "applied"
	if p := html(t, Config(bound)); strings.Index(p, "Latest plan") < strings.Index(p, ">Repository</dt>") {
		t.Errorf("a decided plan comes before the facts:\n%s", p)
	}
}

// The bind dialog says "No repositories found" when the connector chosen (the
// first by default) lists none, even though another connector lists some.
func TestPickerEmptyConnectorNote(t *testing.T) {
	v := PickerView{
		Connectors: []Option{{Value: "c1", Label: "one"}, {Value: "c2", Label: "two"}},
		Repos:      []c.RepoOption{{FullName: "a/b", ConnectorID: "c2"}},
	}
	if out := html(t, Picker(v)); strings.Contains(out, "data-no-repos hidden") || !strings.Contains(out, "data-no-repos") {
		t.Errorf("first connector is empty, the note shows:\n%s", out)
	}
	v.Connector = "c2"
	if out := html(t, Picker(v)); !strings.Contains(out, "data-no-repos hidden") {
		t.Errorf("c2 lists a repo, the note is hidden:\n%s", out)
	}
}

func TestShareTestLine(t *testing.T) {
	ok := html(t, ShareTestLine(ShareTestView{Slug: "media", OK: true, Msg: "Readable from n1."}))
	if !strings.Contains(ok, `id="share-test-media"`) || !strings.Contains(ok, "Readable from n1.") || strings.Contains(ok, "<details") {
		t.Errorf("ok:\n%s", ok)
	}
	bad := html(t, ShareTestLine(ShareTestView{Slug: "media", Msg: "The NAS refused access.", Raw: "permission denied"}))
	if !strings.Contains(bad, "The NAS refused access.") || !strings.Contains(bad, "<details") || !strings.Contains(bad, "permission denied") {
		t.Errorf("bad:\n%s", bad)
	}
	row := html(t, Shares(SharesView{Rows: []ShareRow{{Slug: "media", Test: "/o/shares/s1/test"}}}))
	if !strings.Contains(row, `hx-post="/o/shares/s1/test"`) || !strings.Contains(row, `hx-target="#share-test-media"`) || !strings.Contains(row, "hx-disabled-elt") {
		t.Errorf("row:\n%s", row)
	}
	if strings.Contains(html(t, Shares(SharesView{Rows: []ShareRow{{Slug: "media"}}})), ">Test<") {
		t.Error("Test button shown to a non-owner")
	}
}
