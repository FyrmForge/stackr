package admin

import (
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func renderConfig(t *testing.T, v ConfigView) string {
	t.Helper()
	var b strings.Builder
	if err := Config(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The server's Config tab: no connector links to Connectors; unbound shows
// the form; bound shows the facts with the form folded; a waiting plan
// comes before them.
func TestServerConfigTabStates(t *testing.T) {
	none := renderConfig(t, ConfigView{Base: Base, Export: "/x"})
	if !strings.Contains(none, `hx-get="`+Href("connectors")+`"`) || !strings.Contains(none, ">Connectors</a>") {
		t.Errorf("no connector, no link:\n%s", none)
	}
	v := ConfigView{Base: Base, Export: "/x", Connectors: []ConnectorOption{{"k", "main"}}}
	open := renderConfig(t, v)
	if !strings.Contains(open, `name="repo"`) || strings.Contains(open, ">Change</summary>") || strings.Contains(open, "Last plan") {
		t.Errorf("unbound:\n%s", open)
	}
	v.Repo, v.Branch = "acme/srv", "master"
	b := renderConfig(t, v)
	for _, w := range []string{">acme/srv<", ">master", "stackr-server.yml", ">Off", ">Change</summary>", ">Export</summary>"} {
		if !strings.Contains(b, w) {
			t.Errorf("bound lacks %q:\n%s", w, b)
		}
	}
	if strings.Index(b, "<details") > strings.Index(b, `name="repo"`) {
		t.Errorf("bound: the form is not folded:\n%s", b)
	}
	v.Latest = &ConfigPlan{Row: c.PlanRow{Status: "pending", Summary: "1 to add"}}
	if p := renderConfig(t, v); strings.Index(p, "Latest plan") > strings.Index(p, ">Repository</dt>") {
		t.Errorf("a waiting plan is not first:\n%s", p)
	}
}
