package proxy

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// Routes sit flat in their card.
func TestRoutesAreFlat(t *testing.T) {
	var b strings.Builder
	if err := routes(View{Rows: []Route{{Host: "a.dev", Tile: "api", Port: "80"}}}).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	uitest.NoPanelInCard(t, "<section>"+b.String()+"</section>")
}
