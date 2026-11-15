package components

import (
	"context"
	"net/url"
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
		`class="from-tag" data-find-skip><span class="sr-only"> </span>from env</span>`,
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
		"Every org", "Only these orgs", "No org (admins only)", "Server admins only.",
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
	for _, want := range []string{"Danger zone", "/acme/home", "Backups are kept.", `<div class="mt-3 empty:hidden"></div>`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in\n%s", want, html)
		}
	}
}

// A refused save draws its row open with what was typed: the reason under
// the field it names, or over the row when no field there takes it. The
// other rows draw their stored values; the summaries are the caller's.
func TestAccordionRefused(t *testing.T) {
	acc := func(r *Refused) string {
		var b strings.Builder
		v := SettingsAcc{ID: "s", Open: "a", Rows: []AccRow{
			{Key: "a", Title: "A", Summary: "stored a", Body: Field(FieldView{Name: "name", Label: "Name", Value: "old"})},
			{Key: "b", Title: "B", Summary: "stored b", Body: Field(FieldView{Name: "name", Label: "Name", Value: "kept"})},
		}}
		if err := SettingsAccordion(v).Render(WithRefused(context.Background(), r), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	html := acc(&Refused{Row: "b", Field: "name", Msg: "Give it a name.", Vals: url.Values{"name": {"typed"}}})
	a, b, _ := strings.Cut(html, `id="s-b"`)
	for _, w := range []string{`data-acc="b" open`, `value="typed"`, `data-has-error="true" aria-live="polite">Give it a name.`, "stored b"} {
		if !strings.Contains(b, w) {
			t.Errorf("refused row lacks %q\n%s", w, b)
		}
	}
	if strings.Contains(a, " open>") || !strings.Contains(a, `value="old"`) || strings.Contains(a, "Give it a name.") || strings.Contains(b, `role="alert"`) {
		t.Errorf("the other row is touched, or the reason shows twice\n%s", html)
	}
	html = acc(&Refused{Row: "b", Field: "replicas", Msg: "One replica only.", Vals: url.Values{"name": {"typed"}}})
	if !strings.Contains(html, `role="alert">One replica only.</div>`) || strings.Count(html, "One replica only.") != 1 {
		t.Errorf("a reason no field takes goes over the row, once\n%s", html)
	}
}

// A found read-only row has no control, so the row itself takes focus.
func TestFoundReadOnlyRow(t *testing.T) {
	row := func(found bool) string {
		return render(t, readOnlyRow(SettingRowView{Key: "k", Label: "K", Value: "v", ReadOnly: true, Found: found}))
	}
	if h := row(true); !strings.Contains(h, `tabindex="-1"`) || !strings.Contains(h, "autofocus") || !strings.Contains(h, "is-found") {
		t.Errorf("found row = %s", h)
	}
	if h := row(false); strings.Contains(h, "tabindex") || strings.Contains(h, "autofocus") {
		t.Errorf("plain row = %s", h)
	}
}

// settings-find matches a field by data-find-key: a SettingsForm row carries
// its Key, a Field only the Key it is given.
func TestFindKey(t *testing.T) {
	for name, h := range map[string]string{
		"edit row":      render(t, editRow("f", SettingRowView{Key: "mem_limit_mb", Label: "Memory limit"})),
		"read-only row": render(t, readOnlyRow(SettingRowView{Key: "mem_limit_mb", Label: "Memory limit", ReadOnly: true})),
		"field":         render(t, Field(FieldView{Name: "mem", Key: "mem_limit_mb", Label: "Memory limit"})),
	} {
		if !strings.Contains(h, `data-find-key="mem_limit_mb"`) {
			t.Errorf("%s: no data-find-key in\n%s", name, h)
		}
		if strings.Contains(h, `aria-label="mem_limit_mb"`) {
			t.Errorf("%s: the key is in an aria-label", name)
		}
	}
	if h := render(t, Field(FieldView{Name: "name", Label: "Name"})); strings.Contains(h, "data-find-key") {
		t.Errorf("a field with no key carries one:\n%s", h)
	}
}
