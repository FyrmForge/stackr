package components

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The custom-element whitelist (docs/rewrite/tasks/step-6.md, ui-plan §3):
// one TS file per tag with its line budget, nothing else. Adding a tag is a
// plan item, not a build decision.
var elements = map[string]int{
	"confirm-dialog": 150,
	"flash-toast":    150,
	"graph-canvas":   400,
	"graph-node":     150,
	"log-pane":       150,
	"side-drawer":    150,
	"theme-toggle":   150,
}

// The contract each element keeps with its templ wrapper: the attribute,
// event and selector names the source must carry. Session C's templ
// renders against these (step-6.md "Element contract").
var contract = map[string][]string{
	"confirm-dialog": {`"confirmed"`, `"word"`, `"open"`, "[data-open]", "[data-cancel]", "[data-confirm]", "[data-word]"},
	"flash-toast":    {`"kind"`, "htmx:responseError"},
	"graph-canvas": {
		`"selection-changed"`,
		"detail: { ids:",
		`"scale"`,
		`"snap"`,
		`"straight"`,
		`"fan-out"`,
		`"arrows"`,
		`"hover-focus"`,
		`"nooverlap"`,
		`"badges"`,
		`"boundary"`,
		`"legend"`,
		`"focus"`,
		`"selected"`,
		`"lit"`,
		`"data-lit"`,
		`"focusing"`,
		"svg[data-edges] path[data-from]",
		"svg[data-lanes] g[data-from]",
		"dataset.from",
		"dataset.to",
		`"node-id"`,
		"input[data-look]",
		"[data-zoom]",
		`"data-marquee"`,
		"--px",
		"--py",
		"--s",
	},
	"graph-node": {
		`"node-moved"`,
		`"x"`,
		`"y"`,
		`"w"`,
		`"h"`,
		`"system"`,
		`"static"`,
		`"selected"`,
		`"dragging"`,
		`"snap"`,
		`"divider"`,
		`"nooverlap"`,
		":scope > input[name=${k}]",
		"stopImmediatePropagation",
		"--x",
		"--y",
	},
	"log-pane": {`"level"`, `"search"`, "[data-lines]"},
	"side-drawer": {
		`"drawer-closed"`,
		`"open"`,
		`"tab"`,
		"#drawer-body",
		"[data-close]",
		"[data-backdrop]",
		"history.replaceState",
		`"drawer"`,
	},
	"theme-toggle": {`"theme"`},
}

func TestElementWhitelist(t *testing.T) {
	files, err := filepath.Glob("../../../ui/ts/*.ts")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, strings.TrimSuffix(filepath.Base(f), ".ts"))
	}
	want := slices.Sorted(maps.Keys(elements))
	if !slices.Equal(got, want) {
		t.Fatalf("ui/ts holds %v, the whitelist is %v", got, want)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		tag := strings.TrimSuffix(filepath.Base(f), ".ts")
		if n := strings.Count(src, "\n"); n >= elements[tag] {
			t.Errorf("%s: %d lines, the budget is under %d", f, n, elements[tag])
		}
		for _, banned := range []string{
			"import ",
			"import(",
			"fetch(",
			"XMLHttpRequest",
			"attachShadow",
			"innerHTML",
			"outerHTML",
			"eval(",
		} {
			if strings.Contains(src, banned) {
				t.Errorf("%s contains %q", f, banned)
			}
		}
		if strings.Count(src, "customElements.define(") != 1 ||
			!strings.Contains(src, `customElements.define("`+tag+`"`) {
			t.Errorf("%s must define exactly one tag, %q", f, tag)
		}
		for _, name := range contract[tag] {
			if !strings.Contains(src, name) {
				t.Errorf("%s: contract name %q is missing", f, name)
			}
		}
	}
}
