package components

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// A Section's note sits right after the form that posted, before what
// follows it in the card; a card whose form did not post stays clean.
func TestSectionNoteAfterItsForm(t *testing.T) {
	body := templ.Raw(`<form hx-post="/a/save"><button>Save</button></form><p>Status: on.</p>`)
	other := templ.Raw(`<form hx-post="/b/save"></form>`)
	ctx := WithActed(context.Background(), &Acted{Path: "/a/save", Note: "Saved."})
	var b strings.Builder
	for _, c := range []templ.Component{other, body} {
		if err := Section("x", "", "").Render(templ.WithChildren(ctx, c), &b); err != nil {
			t.Fatal(err)
		}
	}
	got := b.String()
	note, form, status := strings.Index(got, "Saved."), strings.Index(got, `hx-post="/a/save"`), strings.Index(got, "Status: on.")
	if strings.Count(got, "Saved.") != 1 || note < form || note > status {
		t.Errorf("note not between its form and the status line:\n%s", got)
	}
}
