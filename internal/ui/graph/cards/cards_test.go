package cards

import (
	"context"
	"strings"
	"testing"
	"time"

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
		"ref":      "operated tile, open home",
		"vars":     "open settings",
		"secrets":  "open settings",
	}
	for _, k := range []string{
		"service",
		"cron",
		"function",
		"operated",
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
// The operated card's footer names extension updates while there are any.
func TestFooterExtUpdates(t *testing.T) {
	foot := func(n int) string {
		return render(t, Footer("n1", FooterView{Kind: "operated", Status: "running", ExtUpdates: n}))
	}
	if !strings.Contains(foot(1), "Extension updates") || strings.Contains(foot(0), "Extension updates") {
		t.Error("the footer chip follows the count")
	}
}

// A deploy in flight reads beside the live state, not over it.
func TestProgressLine(t *testing.T) {
	since := time.Now().Add(-9*time.Minute - 20*time.Second - 500*time.Millisecond)
	for _, c := range []struct{ status, under, want string }{
		{"building", "running", "updating <local-time"},
		{"building", "", ">Building<"},
	} {
		out := render(t, Footer("n1", FooterView{Kind: "service", Status: c.status, Under: c.under, Since: since}))
		if !strings.Contains(out, c.want) || !strings.Contains(out, ">00:00</local-time>") {
			t.Errorf("%s over %q: %s", c.status, c.under, out)
		}
	}
	out := render(t, Footer("n1", FooterView{Kind: "service", Status: "building", Under: "running", Since: since}))
	if !strings.Contains(out, "text-rw-success") || !strings.Contains(out, ">Online<") || !strings.Contains(out, "updating <local-time") {
		t.Errorf("online with the build muted beside it: %s", out)
	}
	// the same job a poll later renders the same bytes: the canvas dedupes it
	again := render(t, Footer("n1", FooterView{Kind: "service", Status: "building", Under: "running", Since: since}))
	if again != out {
		t.Errorf("footer differs between polls:\n%s\n%s", out, again)
	}
	if elapsed(time.Now().Add(-3725*time.Second)) != "1:02:05" {
		t.Error("an hour reads h:mm:ss")
	}
}

// A live job shows its step: a hollow dot and no clock while queued, a
// spinner and the clock while it runs.
func TestFooterPhase(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	q := render(t, Footer("n1", FooterView{Kind: "service", Status: "queued", Phase: "Queued", Since: since}))
	if !strings.Contains(q, ">Queued<") || strings.Contains(q, "local-time") || !strings.Contains(q, "border border-rw-faint") {
		t.Errorf("queued: %s", q)
	}
	r := render(t, Footer("n1", FooterView{Kind: "service", Status: "building", Phase: "Pulling image", Since: since}))
	if !strings.Contains(r, ">Pulling image<") || !strings.Contains(r, "animate-spin") || !strings.Contains(r, "<local-time") {
		t.Errorf("running: %s", r)
	}
}

// A job that waits for a dependency is a faint word with a hollow dot and
// no clock, not a running step.
func TestFooterWaitsForDependency(t *testing.T) {
	out := render(t, Footer("n1", FooterView{Kind: "service", Status: "building", Phase: "Waiting for pg to be healthy", Since: time.Now().Add(-time.Minute)}))
	if !strings.Contains(out, ">Waiting for pg to be healthy<") || strings.Contains(out, "local-time") || strings.Contains(out, "animate-spin") || !strings.Contains(out, "border border-rw-faint") {
		t.Errorf("waiting for a dependency: %s", out)
	}
}

// A failed deploy shows the end of its error above the footer, the whole
// last line in the title, until a later job replaces it.
func TestFooterFail(t *testing.T) {
	long := "build media-bootstrap: " + strings.Repeat("x", 60) + ": exit status 1"
	out := render(t, Footer("n1", FooterView{Kind: "service", Status: "failed", Fail: "step 1\n" + long}))
	if !strings.Contains(out, `title="`+long+`"`) || !strings.Contains(out, ">...") || !strings.Contains(out, "exit status 1</p>") || !strings.Contains(out, "Deploy failed") {
		t.Errorf("failed: %s", out)
	}
	if ok := render(t, Footer("n1", FooterView{Kind: "service", Status: "running"})); strings.Contains(ok, "node-fail") {
		t.Errorf("a healthy tile shows a failure line: %s", ok)
	}
	if s, f := FailLine("short"); s != "short" || f != "short" {
		t.Errorf("FailLine(short) = %q, %q", s, f)
	}
}

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

// A stack card's push failure links to its job log from the footer.
func TestFooterFailJobLog(t *testing.T) {
	out := render(t, Footer("stack:1", FooterView{Kind: "stack", Status: "pushfailed", Fail: "denied", FailHref: "/acme/-/jobs/j1"}))
	if !strings.Contains(out, `href="/acme/-/jobs/j1"`) || !strings.Contains(out, "Job log") {
		t.Errorf("footer = %s", out)
	}
}

// A stack card's note says the build in flight with its job log link.
func TestFooterNote(t *testing.T) {
	out := render(t, Footer("stack:1", FooterView{Kind: "stack", Status: "running", Note: "Building at f9cdc50, started ", NoteAt: time.Now(), NoteHref: "/acme/-/jobs/j1", NoteLink: "Job log"}))
	for _, want := range []string{"Building at f9cdc50, started ", `href="/acme/-/jobs/j1"`, "Job log"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

// The internet box reads Live while its egress carries traffic.
func TestFooterLive(t *testing.T) {
	if out := render(t, Footer("n1", FooterView{Kind: "internet", Status: "live"})); !strings.Contains(out, ">Live<") {
		t.Errorf("live internet footer: %s", out)
	}
}

func TestPausedWord(t *testing.T) {
	if tone, text := word("paused", ""); tone != "warn" || text != "Paused" {
		t.Fatalf("%q %q", tone, text)
	}
}

// A volume with no schedule says so, on its card and on its sub-tile; a
// removed one is drawn faded.
func TestVolumeNoBackupAndFaded(t *testing.T) {
	out := render(t, Card(CardView{ID: "v1", Kind: "volume", Name: "uploads", Detail: "Removed on 02-01", NoBackup: true, Faded: true}))
	for _, w := range []string{"No backup", "Removed on 02-01", "opacity-70"} {
		if !strings.Contains(out, w) {
			t.Errorf("card lacks %q\n%s", w, out)
		}
	}
	plain := render(t, Card(CardView{ID: "v1", Kind: "volume", Name: "uploads", Detail: "detached"}))
	if strings.Contains(plain, "No backup") || strings.Contains(plain, "opacity-70") {
		t.Errorf("a scheduled volume reads no backup or faded\n%s", plain)
	}
	sub := render(t, Subs(CardView{Subs: []SubView{{ID: "v1", Kind: "volume", Label: "uploads", Detail: "/data", NoBackup: true}}}))
	if !strings.Contains(sub, "No backup") || !strings.Contains(sub, "/data") {
		t.Errorf("sub-tile\n%s", sub)
	}
}
