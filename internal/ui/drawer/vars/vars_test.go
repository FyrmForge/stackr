package vars

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestEditor(t *testing.T) {
	var b strings.Builder
	_ = Editor(View{
		Scope: "org acme",
		Error: "nope",
		Editor: c.ParamEditorView{
			Action:  "/o/-/vars",
			Params:  []c.ParamRowView{{Collection: "app", Name: "mode", Value: "prod"}},
			Secrets: []c.SecretRowView{{Collection: "db", Name: "pass", Set: true}},
		},
	}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{
		`id="vars-editor"`,
		"org acme",
		"nope",
		`name="param.app.mode" value="prod"`,
		`name="secret.db.pass"`,
		"unchanged",
		"••••••••",
		"+ Add variable",
		`name="new_name"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
}

func TestEditorLead(t *testing.T) {
	var b strings.Builder
	_ = Editor(View{Scope: "the server", Lead: "Only the server file reads these."}).Render(context.Background(), &b)
	got := b.String()
	if !strings.Contains(got, "Only the server file reads these.") || strings.Contains(got, "redeploys") {
		t.Errorf("lead not used in\n%s", got)
	}
}

// A grid row set at this level is a changed field, so Only changed keeps
// the variables a scope sets itself.
func TestGridRowChanged(t *testing.T) {
	var b strings.Builder
	_ = Editor(View{Grid: &Grid{
		ReadOnly: true,
		Cols:     []Col{{Token: "env:1", Label: "dev"}},
		Groups:   []Group{{Name: "app", Rows: []Row{{Name: "mode", Cells: []Cell{{Value: "prod", Set: true}}}, {Name: "gone", Cells: []Cell{{}}}}}},
	}}).Render(context.Background(), &b)
	if n := strings.Count(b.String(), "data-field data-changed"); n != 1 {
		t.Errorf("%d changed rows, want the one set:\n%s", n, b.String())
	}
}
