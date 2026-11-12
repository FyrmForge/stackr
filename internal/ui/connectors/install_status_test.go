package connectors

import (
	"context"
	"strings"
	"testing"
)

// A good install answer stays for good; one that did not answer, or found
// nothing installed, asks again every 60s.
func TestInstallStatusRechecks(t *testing.T) {
	render := func(repos int, failed bool) string {
		var b strings.Builder
		if err := InstallStatus("c1", repos, failed, "/x/c1/installed").Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if got := render(2, false); !strings.Contains(got, "hx-preserve") || strings.Contains(got, "hx-post") {
		t.Errorf("a good answer polls: %s", got)
	}
	for name, got := range map[string]string{"failed": render(0, true), "none": render(0, false)} {
		for _, want := range []string{"hx-preserve", `hx-post="/x/c1/installed"`, `hx-trigger="every 60s"`} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in %s", name, want, got)
			}
		}
	}
}

// A row with a known install answer renders it and fires no load-time check,
// so the list's poll never re-asks; one without asks once as it loads.
func TestRowChecksOnlyWithoutAnswer(t *testing.T) {
	render := func(r Row) string {
		var b strings.Builder
		if err := row(r).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	base := Row{ID: "c1", Kind: "GitHub", Name: "n", Check: "/x/c1/installed"}
	if got := render(base); !strings.Contains(got, `hx-trigger="load"`) {
		t.Errorf("unanswered row does not check: %s", got)
	}
	base.Answered, base.Repos = true, 2
	got := render(base)
	if strings.Contains(got, `hx-trigger="load"`) || strings.Contains(got, "hx-post") || !strings.Contains(got, "2 repos") {
		t.Errorf("answered row checks again or lacks the answer: %s", got)
	}
}
