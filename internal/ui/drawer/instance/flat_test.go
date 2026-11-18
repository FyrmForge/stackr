package instance

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The allow list and the env pairs sit flat in their accordion rows.
func TestInstanceSettingsAreFlat(t *testing.T) {
	var b strings.Builder
	v := View{Name: "pg", Base: "/o/s/e/-/instances/pg"}
	s := SettingsView{Allow: []string{"a/*"}, Pairs: []Pair{{From: "dev", To: "stg"}}, Open: "allow"}
	if err := Settings(v, s).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "a/*") || !strings.Contains(b.String(), "stg") {
		t.Fatalf("no rows\n%s", b.String())
	}
	uitest.NoPanelInCard(t, b.String())
}
