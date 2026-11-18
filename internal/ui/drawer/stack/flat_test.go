package stack

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The release order sits flat in the accordion.
func TestOrderIsFlat(t *testing.T) {
	var b strings.Builder
	v := SettingsView{Name: "s", Open: "order", Order: OrderView{Rungs: []Rung{{ID: "1", Name: "dev"}, {ID: "2", Name: "prod"}}}}
	if err := Settings(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	uitest.NoPanelInCard(t, b.String())
}
