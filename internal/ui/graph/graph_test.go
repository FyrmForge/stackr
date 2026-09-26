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
	route := strings.Index(legend, "public route")
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
