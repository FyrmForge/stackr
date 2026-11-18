package access

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The controller note inside an approval card is no box of its own.
func TestApproveNoteIsFlat(t *testing.T) {
	var b strings.Builder
	v := ApproveView{ID: "a", Action: "/approve", Name: "shop", Pending: []string{"web network:host"},
		Tiles:      Group(nil, []string{"web network:host"}),
		Controller: &ControllerWarn{Node: "home", Envs: []ControllerEnv{{Name: "production", Move: "/m"}}}}
	if err := Approve(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	uitest.NoPanelInCard(t, "<section>"+b.String()+"</section>")
}
