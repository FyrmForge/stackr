package components

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Open picks the one row open on load; a Changed row carries the dot and
// data-changed for Only changed; the summary sits in the closed row.
func TestSettingsAccordion(t *testing.T) {
	html := render(t, SettingsAccordion(SettingsAcc{
		ID:   "tile-settings",
		Open: "network",
		Rows: []AccRow{
			{Key: "general", Title: "General", Summary: "web, service"},
			{Key: "network", Title: "Network", Summary: "8080 http, 2 domains", Changed: true,
				Body: Field(FieldView{Name: "port", Label: "Container port", Changed: true, From: "env"})},
		},
	}))
	for _, want := range []string{
		`<settings-find id="tile-settings"`,
		`<label for="tile-settings-find" class="sr-only">Find a setting</label>`,
		`aria-live="polite" data-find-status`,
		`id="tile-settings-network" name="tile-settings" class="panel group" data-acc="network" data-changed open>`,
		`id="tile-settings-general" name="tile-settings" class="panel group" data-acc="general">`,
		`8080 http, 2 domains</span>`,
		`<div data-field data-changed>`,
		`class="from-tag" data-find-skip>from env</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in\n%s", want, html)
		}
	}
	if n := strings.Count(html, `class="changed-dot"`); n != 2 {
		t.Errorf("changed dots: %d, want 2 (the Network row and its port)", n)
	}
	if n := strings.Count(html, " open>"); n != 1 {
		t.Errorf("open rows: %d, want 1", n)
	}
}

// A field left at its zero value renders neither mark.
func TestFieldNoMarks(t *testing.T) {
	html := render(t, Field(FieldView{Name: "name", Label: "Name"}))
	if strings.Contains(html, "data-changed") || strings.Contains(html, "changed-dot") || strings.Contains(html, "from-tag") {
		t.Errorf("zero-value field has marks:\n%s", html)
	}
}

// The org boxes post only under Only these orgs; the caller names the fields.
func TestShare(t *testing.T) {
	orgs := []ShareOrg{{ID: "o1", Name: "acme", On: true}, {ID: "o2", Name: "smoke"}}
	only := render(t, Share(ShareView{ModeName: "share", OrgName: "org", Mode: ShareOnly, Orgs: orgs}))
	for _, want := range []string{
		`name="share" value="only" checked`,
		`name="org" value="o1" checked class=`,
		`name="org" value="o2" class=`,
		"Every org", "Only these orgs", "Server only",
	} {
		if !strings.Contains(only, want) {
			t.Errorf("only: missing %q in\n%s", want, only)
		}
	}
	if strings.Contains(only, "disabled") {
		t.Errorf("only: an org box is disabled:\n%s", only)
	}
	all := render(t, Share(ShareView{ModeName: "share", OrgName: "org", Mode: ShareAll, Orgs: orgs}))
	if !strings.Contains(all, `name="share" value="all" checked`) || strings.Count(all, "disabled") != 2 {
		t.Errorf("all: want every org box disabled:\n%s", all)
	}
	if none := render(t, Share(ShareView{ModeName: "share", OrgName: "org", Mode: ShareServer})); !strings.Contains(none, "No organizations yet.") {
		t.Errorf("no orgs:\n%s", none)
	}
}

// One danger zone: the heading, the path and what is kept; no children, no
// empty button row.
func TestDangerZone(t *testing.T) {
	html := render(t, DangerZone("Deleting this stack removes it.", "/acme/home", "Backups are kept."))
	for _, want := range []string{"Danger zone", "/acme/home", "Backups are kept."} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in\n%s", want, html)
		}
	}
}
