package placement_test

import (
	"strings"
	"testing"

	ui "github.com/FyrmForge/stackr/internal/ui/placement"
	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The exit ladder sits flat in the Exits card.
func TestExitsAreFlat(t *testing.T) {
	out := render(t, ui.Placement(ui.View{Base: "/p", Scope: "org",
		Rungs: []ui.Rung{{NodeID: "n1", Name: "eu-1", State: "in use", Own: true}}}))
	if !strings.Contains(out, "eu-1") {
		t.Fatal("no rung")
	}
	uitest.NoPanelInCard(t, out)
}
