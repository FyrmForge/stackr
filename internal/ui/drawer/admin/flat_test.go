package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/ui/access"
	c "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The admin tabs hold their rows flat: no panel box inside a section card.
func TestAdminSectionsAreFlat(t *testing.T) {
	var all strings.Builder
	pages := []string{
		render(t, Access(AccessView{
			Waiting: []WaitingStack{{Where: "o / s", Form: access.ApproveView{}}},
			Dests:   []DestAccess{{Text: "dest"}},
			Rows:    []AccessRow{{Org: "o", Stack: "s", Tile: "t"}},
		})),
		render(t, Users(UsersView{Rows: []User{{Email: "a@b.c", Name: "A"}}})),
		render(t, Routes(RoutesView{Rows: []Route{{Host: "h", Mode: "http"}}})),
		render(t, Backups(BackupsView{Rows: []Backup{{Status: "done", When: time.Now()}}, Dests: c.DestsView{Rows: []c.DestView{{Name: "d"}}}})),
		render(t, Placements(PlacementsView{Rows: []PlacementRow{{Name: "p"}}})),
		render(t, Security(SecurityView{Rows: []SecurityRow{{When: time.Now(), Actor: "a", Action: "x", Details: []SecurityDetail{{Label: "l", Value: "v"}}}}})),
		render(t, Extensions(ExtensionsView{Builtins: []RecipeRow{{Name: "pg"}}, Own: []RecipeRow{{Name: "mine"}}})),
		render(t, Overview(OverviewView{Problems: []ProblemRow{{Message: "m"}}})),
		renderUpdate(t, UpdateView{Version: "1.0", Workers: &WorkersView{Rows: []WorkerRow{{Name: "h4"}}}}),
	}
	for _, p := range pages {
		all.WriteString(p)
	}
	uitest.NoPanelInCard(t, all.String())
}
