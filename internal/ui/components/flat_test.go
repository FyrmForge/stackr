package components

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// A plan review, the destinations list and the variables editor are drawn
// inside a card: their rows and notes are flat, no panel box inside.
func TestSharedListsAreFlat(t *testing.T) {
	var b strings.Builder
	b.WriteString("<section>")
	b.WriteString(render(t, PlanReview(PlanReviewView{
		Plan: PlanView{
			Gaps:    []GapView{{Kind: "secret", Label: "db.password", Field: "f", Post: "/p"}},
			Changes: []ChangeView{{Kind: "setting", Field: "workers", New: "3", Impact: "moves"}, removal},
		},
		Approve: "/a",
	})))
	b.WriteString(render(t, Dests(DestsView{Rows: []DestView{{Name: "dest1"}}})))
	b.WriteString(render(t, ParamEditor(ParamEditorView{
		Action:  "/v",
		Params:  []ParamRowView{{Collection: "app", Name: "mode", Value: "prod"}},
		Secrets: []SecretRowView{{Collection: "db", Name: "pass"}},
	})))
	b.WriteString("</section>")
	for _, want := range []string{"db.password", "workers", "dest1", "mode", "pass"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %s rendered", want)
		}
	}
	uitest.NoPanelInCard(t, b.String())
}

// The bar's Settings link sits at the right on every level, with the page
// actions, never beside the crumbs.
func TestSettingsLinkSameSideAtEveryLevel(t *testing.T) {
	for n := 1; n <= 4; n++ {
		crumbs := make([]Link, n)
		for i := range crumbs {
			crumbs[i] = Link{Label: "c", Href: "/c"}
		}
		out := render(t, topBar(Shell{Crumbs: crumbs, Settings: "/s", SettingsPush: "/p"}, false))
		_, right, ok := strings.Cut(out, `class="ml-auto flex`)
		if !ok || !strings.Contains(right, ">Settings</button>") || strings.Count(out, ">Settings</button>") != 1 {
			t.Errorf("%d crumbs: Settings not once on the right\n%s", n, out)
		}
	}
}
