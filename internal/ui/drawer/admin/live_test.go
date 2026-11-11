package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// Every admin tab that shows moving state carries a poller on its own GET.
func TestAdminTabsAreLive(t *testing.T) {
	for name, tc := range map[string]struct {
		c    templ.Component
		want string
	}{
		"overview":   {Overview(OverviewView{}), `hx-select="#tab-overview"`},
		"nodes":      {Nodes(NodesView{}), `hx-select="#tab-nodes"`},
		"placements": {Placements(PlacementsView{}), `hx-select="#placements-list"`},
		"backups":    {Backups(BackupsView{}), `hx-select="#tab-backups"`},
		"access":     {Access(AccessView{}), `hx-select="#tab-access"`},
		"users":      {Users(UsersView{}), `hx-select="#tab-users"`},
		"routes":     {Routes(RoutesView{}), `hx-select="#tab-routes"`},
	} {
		var b strings.Builder
		if err := tc.c.Render(context.Background(), &b); err != nil {
			t.Fatal(name, err)
		}
		if !strings.Contains(b.String(), tc.want) || !strings.Contains(b.String(), "every 3s") {
			t.Errorf("%s is not live", name)
		}
	}
	var b strings.Builder
	_ = Nodes(NodesView{Adder: &AddNodeView{}}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "every 3s") {
		t.Error("nodes polls while the add form is open")
	}
}
