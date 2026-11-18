package setup

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The wizard shell is one card: its lists, notices and choices sit flat in
// it, no panel box inside.
func TestSetupShellIsFlat(t *testing.T) {
	var b strings.Builder
	for _, comp := range []interface {
		Render(ctx context.Context, w io.Writer) error
	}{
		ConnectorBody(GitView{Connected: true, Repos: []Repo{{FullName: "a/b"}}}),
		Plan(PlanView{Parsed: true, Error: "boom", Errors: []string{"bad"}, Warnings: []string{"hmm"},
			Gaps: []Gap{{Label: "tok", Field: "f", Post: "/p"}}, Changes: []Change{{Kind: "tile", Tile: "api"}}, Work: "applying"}),
		Team(TeamView{Candidates: []Candidate{{Name: "A", Email: "a@b.c"}}}),
		Done(DoneView{Name: "acme", Rows: []Row{{Label: "Name"}}}),
		ServerObserve(ServerObserveView{}),
		ServerSummary(ServerSummaryView{Rows: []SummaryRow{{Label: "Domain", Href: "/x"}}}),
		ServerNodesPanel(NodesPanel{Base: "/s", Pending: []admin.PendingRow{{Name: "w1"}}}),
	} {
		if err := comp.Render(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"a/b", "bad", "tok", "a@b.c", "Domain", "w1"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %s rendered", want)
		}
	}
	uitest.NoPanelInCard(t, b.String())
}
