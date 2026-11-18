package connectors

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// Connector rows sit flat in the page's card.
func TestConnectorRowsAreFlat(t *testing.T) {
	var b strings.Builder
	v := ListView{Title: "Connectors", Scope: "organization", Rows: []Row{{ID: "k1", Kind: "GitHub", Name: "gh"}}}
	if err := List(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "gh") {
		t.Fatal("no row")
	}
	uitest.NoPanelInCard(t, b.String())
}
