package cluster_test

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/cluster"
)

// A node card's face is a fixed-height column with the footer pinned to its
// bottom edge: the face is the card minus its 2 px border, so the markup's
// height and the placed card's height move together.
func TestNodeCardFooterOnBottomEdge(t *testing.T) {
	m := sample()
	v := cluster.Build(m, cluster.Base, nil, nil)
	faceH := 0
	for _, n := range v.Nodes {
		if n.ID == "node:g" { // no envs: the card is just the face
			faceH = n.H
		}
	}
	if faceH != 164 {
		t.Fatalf("a bare node card is %d px tall, want 164", faceH)
	}
	body := render(t)
	for _, want := range []string{"h-[162px]", "node-footer mt-auto"} {
		if !strings.Contains(body, want) {
			t.Errorf("node card lacks %q", want)
		}
	}
	if strings.Contains(body, "node-footer mt-3") {
		t.Error("the footer still floats on a fixed margin")
	}
}
