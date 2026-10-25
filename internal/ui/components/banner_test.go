package components

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// Shell.NodeDown renders the offline banner under the top bar; empty
// renders none.
func TestNodeDownBanner(t *testing.T) {
	render := func(down string) string {
		var b strings.Builder
		s := Shell{Crumbs: []Link{{Label: "acme", Href: "/acme"}}, NodeDown: down}
		if err := Layout(s, templ.NopComponent).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	msg := "h3 is offline, last seen 14 min ago. Deploys here are refused until it is back."
	if got := render(msg); !strings.Contains(got, `id="node-down"`) || !strings.Contains(got, msg) {
		t.Errorf("no banner:\n%s", got)
	}
	if got := render(""); strings.Contains(got, "node-down") {
		t.Error("an empty NodeDown rendered a banner")
	}
}

// The problem banner draws the first three, then "and N more"; a critical
// one sets the danger tone. It shows on a page with no crumbs too.
func TestProblemBanner(t *testing.T) {
	var ps []Problem
	for i := range 5 {
		ps = append(ps, Problem{Critical: i == 0, Message: "problem " + string(rune('a'+i)), Href: "?drawer=admin&tab=backups", LinkText: "Open backups"})
	}
	var b strings.Builder
	if err := Layout(Shell{Problems: ps, ProblemsMore: "?drawer=admin&tab=backups"}, templ.NopComponent).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	_, banner, _ := strings.Cut(got, `id="problems"`)
	banner, _, _ = strings.Cut(banner, "</div>")
	if !strings.Contains(got, `id="problems" role="alert"`) || !strings.Contains(banner, "bg-rw-danger/10") {
		t.Errorf("no danger banner:\n%s", got)
	}
	if n := strings.Count(banner, "Open backups"); n != 3 || !strings.Contains(banner, "and 2 more") || strings.Contains(banner, "problem d") {
		t.Errorf("%d lines:\n%s", n, banner)
	}
	b.Reset()
	if err := Layout(Shell{}, templ.NopComponent).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `id="problems"`) {
		t.Error("no problems rendered a banner")
	}
}

// A skipped job (its node was offline) reads as a muted word, not a failure.
func TestSkippedJobBadgeIsMuted(t *testing.T) {
	var b strings.Builder
	if err := JobBadge("skipped").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.Contains(got, "skipped") || !strings.Contains(got, "text-rw-muted") {
		t.Errorf("badge = %s", got)
	}
}
