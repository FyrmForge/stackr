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
