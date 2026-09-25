package components

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// A fresh load of ?drawer=&tab= renders the drawer open with its body;
// without one it is closed and empty.
func TestSideDrawerOnLoad(t *testing.T) {
	var b strings.Builder
	if err := Layout(
		Shell{Drawer: templ.Raw("<p>tab body</p>"), Tab: "members"},
		templ.NopComponent,
	).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `<side-drawer open tab="members">`) ||
		!strings.Contains(b.String(), `<div id="drawer-body"><p>tab body</p></div>`) {
		t.Errorf("open drawer:\n%s", b.String())
	}
	b.Reset()
	_ = Layout(Shell{}, templ.NopComponent).Render(context.Background(), &b)
	if !strings.Contains(b.String(), "<side-drawer>") || !strings.Contains(b.String(), `<div id="drawer-body"></div>`) {
		t.Errorf("closed drawer:\n%s", b.String())
	}
}
