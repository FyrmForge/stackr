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
// footer swaps itself, never the drawer.
func TestCardKinds(t *testing.T) {
	for _, k := range []string{"service", "cron", "function", "managed", "slice", "ref", "volume", "proxy", "internet", "vars", "secrets"} {
		out := render(t, Card(CardView{ID: "n1", Kind: k, Name: "x", Drawer: "/d?tab=status", Tab: "status",
			Footer: FooterView{LastRun: "never run", Count: 2}}))
		for _, want := range []string{`data-kind="` + k + `"`, `sse-swap="footer:n1" hx-target="this"`, `hx-push-url="?drawer=n1&amp;tab=status"`, "never run", "2 names"} {
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
	a := render(t, Lanes([]Lane{{From: "proxy", To: "t1", BPS: 12_600}, {From: "t1", To: "p1", BPS: 500}}))
	b := render(t, Lanes([]Lane{{From: "proxy", To: "t1", BPS: 3 << 20}}))
	if !strings.Contains(a, "12.3 KB/s") || !strings.Contains(a, "500 B/s") || !strings.Contains(a, `data-from="t1" data-to="p1"`) {
		t.Errorf("first sample:\n%s", a)
	}
	if !strings.Contains(b, "3.0 MB/s") || strings.Contains(b, "KB/s") || strings.Contains(b, `data-to="p1"`) {
		t.Errorf("second sample:\n%s", b)
	}
	if !strings.Contains(b, `sse-swap="traffic" hx-target="this" hx-swap="outerHTML"`) {
		t.Errorf("lanes do not swap themselves:\n%s", b)
	}
}
