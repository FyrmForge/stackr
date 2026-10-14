package graph

import (
	"context"
	"strings"
	"testing"
)

// The canvas draws each edge with its kind's look, a legend of only the
// kinds present in v0's order, the boundary 120 px past the cards, and an
// env card's hue on its face.
func TestCanvasChrome(t *testing.T) {
	v := View{
		Base: "/acme/shop",
		Show: Show{System: true, Refs: true, Startup: true, Traffic: true},
		Nodes: []Node{
			{ID: "proxy", Kind: "proxy", Name: "proxy", X: 0, Y: 40, W: 220, H: 96, System: true},
			{ID: "env:1", Kind: "env", Name: "dev", X: 400, Y: 200, W: 220, H: 96, Color: "violet", Href: "/acme/shop/dev"},
		},
		Edges: []Edge{
			{Kind: "ref", From: "env:1", To: "proxy"},
			{Kind: "ingress", From: "proxy", To: "env:1"},
		},
		Walled:  true,
		Divider: 300,
		Empty:   "nothing here",
	}
	var b strings.Builder
	if err := Canvas(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	for name, want := range map[string]string{
		"ingress stroke": `data-edge-kind="ingress" data-from="proxy" data-to="env:1" d="M0,0" fill="none" stroke="rgb(var(--rw-accent))"`,
		"ref dash":       `stroke-dasharray="4 5"`,
		"divider":        `<line x1="300" y1="-80" x2="300" y2="416"></line>`,
		"divider label":  `<text x="290" y="-62">server</text>`,
		"hue":            "has-env-c env-c-violet",
		"empty":          "nothing here",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: canvas lacks %q", name, want)
		}
	}
	_, legend, _ := strings.Cut(body, `class="graph-legend"`)
	route := strings.Index(legend, "ingress")
	ref := strings.Index(legend, "reference")
	if route < 0 || ref < route {
		t.Errorf("legend rows out of v0 order: public route at %d, reference at %d", route, ref)
	}
	if strings.Contains(legend, "startup order") {
		t.Error("legend lists a kind the canvas does not draw")
	}
	if got := v.Toggle("refs"); got != "/acme/shop?refs=0" {
		t.Errorf("Toggle(refs) = %q", got)
	}
}

// The compare pill's arrow says a rung with no release has none, not #0.
func TestBehindTitle(t *testing.T) {
	if got := behindTitle(Rung{Release: 0, Behind: true}); got != "No release yet; the env below runs one" {
		t.Errorf("no release = %q", got)
	}
	if got := behindTitle(Rung{Release: 3, Behind: true}); got != "Runs release #3; the env below runs a newer one" {
		t.Errorf("release 3 = %q", got)
	}
}

func render(t *testing.T, v View) string {
	t.Helper()
	var b strings.Builder
	if err := Canvas(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A note rises above the cards (note-kind="note"); a box frame stays behind
// them (note-kind="box"), or it would cover the cards it frames.
func TestNoteKind(t *testing.T) {
	body := render(t, View{Base: "/x", Notes: []Note{{ID: "1", Kind: "note", Text: "hi", W: 160, H: 80}, {ID: "2", Kind: "box", W: 200, H: 140}}})
	for _, want := range []string{`node-id="note:1"`, `note-kind="note"`, `note-kind="box"`} {
		if !strings.Contains(body, want) {
			t.Errorf("canvas lacks %q", want)
		}
	}
	if strings.Count(body, `note-kind="note"`) != 1 {
		t.Error("only the note carries note-kind=note")
	}
}

// A directed, clickable edge renders data-dir and a hit path that opens the
// drawer. The hit path is not an edge: no data-from, no data-edge-kind, or
// paths(), fan-out and light() would treat it as one. It follows the label
// so repath finds the label right after the pipe core.
func TestEdgeDirAndDrawer(t *testing.T) {
	body := render(t, View{Base: "/x", Edges: []Edge{
		{Kind: "tunnel", From: "a", To: "b", Label: "3 ms", Dir: true, Drawer: "/-/cluster/tunnel?a=a&b=b", Push: "?drawer=tunnel:a:b"},
		{Kind: "tunnel", From: "b", To: "c"},
	}})
	tag := func(from string) string {
		_, rest, _ := strings.Cut(body, `data-from="`+from+`"`)
		tag, _, _ := strings.Cut(rest, "></path>")
		return tag
	}
	if !strings.Contains(tag("a"), " data-dir") {
		t.Errorf("edge a-b lacks data-dir:\n%s", body)
	}
	if strings.Contains(tag("b"), "data-dir") {
		t.Error("plain edge b-c has data-dir")
	}
	if n := strings.Count(body, "data-edge-hit"); n != 1 {
		t.Fatalf("want 1 hit path, got %d", n)
	}
	_, rest, _ := strings.Cut(body, "<path data-edge-hit")
	hit, _, _ := strings.Cut(rest, "</path>")
	for _, want := range []string{`d="M0,0"`, `hx-get="/-/cluster/tunnel?a=a&amp;b=b"`, `hx-target="#drawer-body"`, `hx-push-url="?drawer=tunnel:a:b"`} {
		if !strings.Contains(hit, want) {
			t.Errorf("hit path lacks %q: %s", want, hit)
		}
	}
	if strings.Contains(hit, "data-from") || strings.Contains(hit, "data-edge-kind") {
		t.Errorf("hit path must not look like an edge: %s", hit)
	}
	if strings.Index(body, "data-edge-label") > strings.Index(body, "data-edge-hit") {
		t.Error("hit path must come after the label")
	}
	if !strings.Contains(body, `id="tunnel-arrow"`) || !strings.Contains(body, `markerUnits="userSpaceOnUse"`) {
		t.Error("tunnel-arrow marker missing or not in user space units")
	}
}

// Own-app egress is thin and faded, egress through another node is dashed;
// the legend names both.
func TestEgressLooks(t *testing.T) {
	body := render(t, View{Edges: []Edge{{Kind: "egress-own", From: "a", To: "b"}, {Kind: "egress-via", From: "a", To: "c"}}})
	for _, want := range []string{`stroke-opacity="0.35"`, `stroke-dasharray="3 4"`, "egress, own apps", "egress through another node"} {
		if !strings.Contains(body, want) {
			t.Errorf("canvas lacks %q", want)
		}
	}
}

// A card's body is a full-height column and the footer pins to its bottom.
func TestCardFooterOnBottomEdge(t *testing.T) {
	body := render(t, View{Nodes: []Node{{ID: "t", Kind: "env", Name: "web", W: 220, H: 104, Href: "/h"}}})
	for _, want := range []string{"flex h-full flex-col p-3.5", "node-footer mt-auto"} {
		if !strings.Contains(body, want) {
			t.Errorf("card lacks %q", want)
		}
	}
	if strings.Contains(body, "node-footer mt-3") {
		t.Error("footer still uses mt-3")
	}
}
