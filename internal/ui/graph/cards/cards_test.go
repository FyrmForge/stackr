package cards

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

// Every kind draws with its marker; system and ref cards are dashed; the
// footer swaps itself, never the drawer, and reads v0's strip for its kind
// (a volume is a strip itself: no footer).
func TestCardKinds(t *testing.T) {
	foot := map[string]string{
		"cron":     "never run",
		"function": "never run",
		"ref":      "managed tile, open home",
		"vars":     "open settings",
		"secrets":  "open settings",
	}
	for _, k := range []string{
		"service",
		"cron",
		"function",
		"managed",
		"slice",
		"ref",
		"volume",
		"proxy",
		"internet",
		"vars",
		"secrets",
	} {
		out := render(t, Card(CardView{
			ID:     "n1",
			Kind:   k,
			Name:   "x",
			Drawer: "/d?tab=status",
			Tab:    "status",
			Footer: FooterView{Kind: k, LastRun: "never run", Trigger: "manual"},
		}))
		wants := []string{
			`data-kind="` + k + `"`,
			`hx-push-url="?drawer=n1&amp;tab=status"`,
		}
		if k != "volume" {
			word := foot[k]
			if word == "" {
				word = ">idle<"
			}
			wants = append(wants, `sse-swap="footer:n1" hx-target="this"`, word)
		}
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s card lacks %q:\n%s", k, want, out)
			}
		}
		if got := strings.Contains(out, "border-dashed"); got != (k == "proxy" || k == "internet" || k == "ref") {
			t.Errorf("%s card dashed = %v", k, got)
		}
	}
}

// A new traffic sample redraws the lanes whole: the new rate replaces the
// old one, a vanished direction goes.
func TestLanesRepaint(t *testing.T) {
	a := render(t, Lanes([]Lane{
		{From: "proxy", To: "t1", BPS: 12_600},
		{From: "t1", To: "p1", BPS: 500},
	}))
	b := render(t, Lanes([]Lane{{From: "proxy", To: "t1", BPS: 3 << 20}}))
	if !strings.Contains(a, "12.3 KB/s") || !strings.Contains(a, "500 B/s") ||
		!strings.Contains(a, `data-from="t1" data-to="p1"`) {
		t.Errorf("first sample:\n%s", a)
	}
	if !strings.Contains(b, "3.0 MB/s") || strings.Contains(b, "KB/s") || strings.Contains(b, `data-to="p1"`) {
		t.Errorf("second sample:\n%s", b)
	}
	if !strings.Contains(b, `sse-swap="traffic" hx-target="this" hx-swap="outerHTML"`) {
		t.Errorf("lanes do not swap themselves:\n%s", b)
	}
}

// A drill-down card is itself a link: its host is text, never a nested
// <a> (the parser would split the card); the org canvas counts domains.
func TestExposureOnDrillCards(t *testing.T) {
	env := render(t, Footer("env:1", FooterView{Kind: "env", Domain: "a.dev", More: 1, Nav: true}))
	if strings.Contains(env, "<a ") || !strings.Contains(env, ">a.dev<") || !strings.Contains(env, "+1") {
		t.Errorf("env card exposure = %s", env)
	}
	stack := render(t, Footer("stack:1", FooterView{Kind: "stack", Domains: 2, Nav: true}))
	if strings.Contains(stack, "<a ") || !strings.Contains(stack, ">2 domains<") {
		t.Errorf("stack card exposure = %s", stack)
	}
	tile := render(t, Footer("t1", FooterView{Kind: "service", Domain: "a.dev"}))
	if !strings.Contains(tile, `href="https://a.dev"`) {
		t.Errorf("tile exposure lost its link: %s", tile)
	}
}
