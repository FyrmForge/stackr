package vars

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestEditor(t *testing.T) {
	var b strings.Builder
	_ = Editor(View{Scope: "org acme", Error: "nope", Editor: c.ParamEditorView{Action: "/o/-/vars",
		Params: []c.ParamRowView{{Collection: "app", Name: "mode", Value: "prod"}}, Secrets: []c.SecretRowView{{Collection: "db", Name: "pass", Set: true}}}}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{`id="vars-editor"`, "org acme", "nope", `name="param.app.mode" value="prod"`, `name="secret.db.pass"`, "unchanged"} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
}
