package components

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A component call written mid-text ("by x · @When(t)") is printed as text
// by templ; the release row renders the date instead.
func TestReleaseRowRendersDate(t *testing.T) {
	when := time.Date(2026, 10, 24, 18, 2, 0, 0, time.UTC)
	v := ReleasesView{Rows: []ReleaseRow{{Number: "5", By: "git push", When: when}}}
	var b strings.Builder
	if err := Releases(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "@When") || !strings.Contains(b.String(), "Oct 24, 18:02") {
		t.Errorf("release row date not rendered:\n%s", b.String())
	}
}

// literalCall is a templ component call that ended up in a generated string literal.
var literalCall = regexp.MustCompile(`WriteString\(templ_7745c5c3_Buffer, \d+, "[^"]*@[A-Za-z_][A-Za-z0-9_.]*\(`)

// No generated template anywhere prints a component call as text.
func TestNoLiteralComponentCalls(t *testing.T) {
	err := filepath.WalkDir("../..", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, "_templ.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if m := literalCall.Find(b); m != nil {
			t.Errorf("%s prints %q as text", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
