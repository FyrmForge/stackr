package components

import (
	"context"
	"io"
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

// An accordion row with two forms puts a refusal no field took, and a saved
// note, right after the form that posted, not over or under the row.
func TestAccRowAnswerAfterItsForm(t *testing.T) {
	body := templ.Raw(`<form hx-post="/e/pin"><button>Save</button></form><p>between</p><form hx-post="/e/ingress"><button>Save</button></form><p>after</p>`)
	acc := SettingsAccordion(SettingsAcc{ID: "s", Open: "placement", Rows: []AccRow{{Key: "placement", Title: "Placement", Body: body}}})
	check := func(what string, ctx context.Context, words string) {
		t.Helper()
		got := render(t, templ.ComponentFunc(func(_ context.Context, w io.Writer) error { return acc.Render(ctx, w) }))
		at, between, after := strings.Index(got, words), strings.Index(got, "between"), strings.Index(got, "after")
		if at < between || at > after {
			t.Errorf("%s: %q not between the ingress form and what follows:\n%s", what, words, got)
		}
	}
	posted := WithPosted(context.Background(), "/e/ingress")
	check("refused", WithRefused(posted, &Refused{Row: "placement", Msg: "No node there."}), "No node there.")
	check("saved", WithActed(posted, &Acted{Note: "Saved."}), "Saved.")
}
