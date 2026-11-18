package vars

import (
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The variables editor sits in a settings accordion: its rows and the add
// form are flat, no panel box inside.
func TestEditorIsFlat(t *testing.T) {
	var b strings.Builder
	err := Editor(View{
		Scope: "org acme",
		Editor: c.ParamEditorView{
			Action:  "/o/-/vars",
			Params:  []c.ParamRowView{{Collection: "app", Name: "mode", Value: "prod"}},
			Secrets: []c.SecretRowView{{Collection: "db", Name: "pass"}},
		},
	}).Render(t.Context(), &b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "mode") {
		t.Fatal("no row")
	}
	uitest.NoPanelInCard(t, "<section>"+b.String()+"</section>")
}
