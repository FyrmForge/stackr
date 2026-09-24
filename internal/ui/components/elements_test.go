package components

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The custom-element whitelist (docs/rewrite/tasks/step-6.md): one TS file
// per tag, nothing else. Adding a tag is a plan item, not a build decision.
var elements = []string{"confirm-dialog", "flash-toast", "log-pane", "theme-toggle"}

func TestElementWhitelist(t *testing.T) {
	files, err := filepath.Glob("../../../ui/ts/*.ts")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, strings.TrimSuffix(filepath.Base(f), ".ts"))
	}
	if !slices.Equal(got, elements) {
		t.Fatalf("ui/ts holds %v, the whitelist is %v", got, elements)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if n := strings.Count(src, "\n"); n > 300 {
			t.Errorf("%s: %d lines, over the 300 refusal line", f, n)
		}
		for _, banned := range []string{"import ", "import(", "fetch(", "XMLHttpRequest", "attachShadow", "innerHTML"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s contains %q", f, banned)
			}
		}
		tag := strings.TrimSuffix(filepath.Base(f), ".ts")
		if strings.Count(src, "customElements.define(") != 1 || !strings.Contains(src, `customElements.define("`+tag+`"`) {
			t.Errorf("%s must define exactly one tag, %q", f, tag)
		}
	}
}
