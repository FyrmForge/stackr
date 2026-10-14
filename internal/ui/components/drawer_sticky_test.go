package components

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderStr(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The header, strip and tabs sit in one sticky wrapper; the body div stays
// the last child of #drawer-view (side-drawer.ts prepends errors there).
func TestDrawerStickyHead(t *testing.T) {
	out := renderStr(t, Drawer(DrawerView{Node: "n", Title: "Web", Base: "/d", Tabs: []string{"a", "b"}, Tab: "a"}, templ.Raw("<p>BODY</p>")))
	const sticky = `<div class="sticky top-0 z-20 bg-rw-surface">`
	s := strings.Index(out, sticky)
	if s < 0 {
		t.Fatalf("no sticky wrapper:\n%s", out)
	}
	body := strings.Index(out, `<div class="p-6 space-y-4">`)
	for _, part := range []string{`id="drawer-title"`, `<tab-strip`} {
		i := strings.Index(out, part)
		if i < s || i > body {
			t.Errorf("%s not inside the sticky head", part)
		}
	}
	if !strings.HasSuffix(out, "<p>BODY</p></div></div>") {
		t.Errorf("body is not the last child:\n%s", out)
	}
	if !strings.Contains(out, "max-w-5xl") || !strings.Contains(out, "has-[[data-wide]]:max-w-none") {
		t.Errorf("no full-screen centring or data-wide escape:\n%s", out)
	}
}

func TestDrawerHeadSticky(t *testing.T) {
	if out := renderStr(t, DrawerHead("T", "k")); !strings.Contains(out, `<div class="sticky top-0 z-20 bg-rw-surface">`) {
		t.Errorf("DrawerHead not sticky:\n%s", out)
	}
}

// The env colour is a stripe on the bar's bottom edge, not a band.
func TestTopBarEnvStripe(t *testing.T) {
	s := Shell{Crumbs: []Link{{Label: "a", Href: "/a"}, {Label: "b", Href: "/b"}}, EnvColor: "teal"}
	out := renderStr(t, topBar(s, false))
	if strings.Contains(out, "env-band") {
		t.Errorf("env-band still drawn:\n%s", out)
	}
	if !strings.Contains(out, "env-c-teal") || !strings.Contains(out, "shadow-[inset_0_-3px_0_var(--env-c)]") || strings.Contains(out, "border-b") {
		t.Errorf("stripe classes:\n%s", out)
	}
	s.EnvColor = ""
	out = renderStr(t, topBar(s, false))
	if !strings.Contains(out, "border-b border-rw-border") || strings.Contains(out, "inset_0_-3px") {
		t.Errorf("plain bar:\n%s", out)
	}
}
