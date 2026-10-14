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
