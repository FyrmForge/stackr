package volume

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderT(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func wantAll(t *testing.T, where, out string, ws ...string) {
	t.Helper()
	for _, w := range ws {
		if !strings.Contains(out, w) {
			t.Errorf("%s: no %q in\n%s", where, w, out)
		}
	}
}

// onePanel: each card is one box with nothing boxed inside it.
func onePanel(t *testing.T, outs ...string) {
	t.Helper()
	for _, o := range outs {
		if n := strings.Count(o, `class="panel `); n != 1 {
			t.Errorf("%d panel boxes in one card\n%s", n, o)
		}
	}
}

// 19a to 19d, 19m, 19q: the Overview fact, with the used bar in its tone and
// the permanent not-enforced line.
func TestCapFactStates(t *testing.T) {
	none := renderT(t, capFact(&CapFact{Value: "No cap", Used: "14.2 GB", Edit: "/e", Node: "v1"}))
	wantAll(t, "19a", none, "Size cap", "No cap", "default", "Used 14.2 GB", "Edit in Settings", `hx-get="/e"`)
	if strings.Contains(none, "h-1.5") {
		t.Error("19a: a bar with no cap")
	}
	set := renderT(t, capFact(&CapFact{Set: true, Value: "20 GB", Used: "14.2 GB", Pct: 71, Tone: "accent"}))
	wantAll(t, "19b", set, "Used 14.2 GB of 20 GB", "bg-rw-accent", "width:71%")
	if strings.Contains(set, "Edit in Settings") {
		t.Error("a person who cannot edit gets the link")
	}
	wantAll(t, "19c", renderT(t, capFact(&CapFact{Set: true, Value: "15 GB", Used: "14.2 GB", Pct: 95, Tone: "warn"})), "Nearly full. Used 14.2 GB of 15 GB", "bg-rw-warn")
	wantAll(t, "19d", renderT(t, capFact(&CapFact{Set: true, Value: "14.2 GB", Used: "14.2 GB", Pct: 100, Tone: "danger"})), "Used 14.2 GB of 14.2 GB. Full, writes fail.", "bg-rw-danger")
	wantAll(t, "19m", renderT(t, capFact(&CapFact{Set: true, Value: "20 GB", Used: "14.2 GB", Pct: 71, Note: "Not enforced on a folder disk."})), "Not enforced on a folder disk.")
	wantAll(t, "19q", renderT(t, capFact(&CapFact{Note: "Not enforced in Docker storage."})), "Not enforced in Docker storage.")
	onePanel(t, none, set)
}

// 19e to 19l, 19o, 19p: the Settings card as a fact with a pencil, a field,
// and a staged cap with its footer. Nothing is saved until Apply.
func TestCapCardStates(t *testing.T) {
	k := CapCard{Mode: "read", Value: "20 GB", Can: true, Base: "/v"}
	read := renderT(t, capCard(k))
	wantAll(t, "19e", read, "Size cap", "20 GB", `aria-label="Edit size cap"`, `hx-get="/v?tab=settings&amp;cap=edit"`)
	k.Can = false
	if strings.Contains(renderT(t, capCard(k)), "Edit size cap") {
		t.Error("a reader gets the pencil")
	}
	k.Can, k.Note = true, "Not enforced on a folder disk."
	wantAll(t, "19o", renderT(t, capCard(k)), "Not enforced on a folder disk.")

	edit := renderT(t, capCard(CapCard{Mode: "edit", Size: "1500", Unit: "MB", Base: "/v"}))
	wantAll(t, "19f", edit, `value="1500"`, `<option value="MB" selected>`, "Leave empty for no cap.", ">Cancel</button>", `hx-post="/v/cap/stage"`, `hx-trigger="change, submit"`)
	bad := renderT(t, capCard(CapCard{Mode: "edit", Size: "0", Unit: "MB", Err: "Give a size of at least 1 MB, or leave it empty for no cap.", Base: "/v"}))
	wantAll(t, "19j", bad, `aria-invalid="true"`, "!border-rw-danger", "Give a size of at least 1 MB, or leave it empty for no cap.")

	pend := renderT(t, capCard(CapCard{Mode: "pending", Value: "30 GB", Was: "20 GB", MB: "30720", Size: "30", Unit: "GB", Impact: "Applies at once. No restart.", Can: true, Base: "/v"}))
	wantAll(t, "19g", pend, "1 change not saved", "line-through", "20 GB", "30 GB", "Applies at once. No restart.", `aria-label="Undo"`,
		`hx-post="/v/cap"`, "&#34;max_size_mb&#34;:&#34;30720&#34;", "Applying", "Apply", `cap=edit&amp;size=30&amp;unit=GB`)
	err := renderT(t, capCard(CapCard{Mode: "pending", Value: "10 GB", Was: "20 GB", MB: "10240", Err: "The volume already holds more than that cap. Set a larger cap.", Base: "/v"}))
	wantAll(t, "19i", err, "The volume already holds more than that cap. Set a larger cap.", `role="alert"`)
	none := renderT(t, capCard(CapCard{Mode: "pending", Value: "No cap", Was: "20 GB", MB: "", Impact: "Applies at once. No restart.", Base: "/v"}))
	wantAll(t, "19l", none, "No cap", "&#34;max_size_mb&#34;:&#34;&#34;")
	wantAll(t, "19p", renderT(t, capCard(CapCard{Mode: "pending", Value: "30 GB", Was: "20 GB", Impact: "Saved, not enforced on a folder disk.", Base: "/v"})), "Saved, not enforced on a folder disk.")
	onePanel(t, read, edit, bad, pend, err, none)
}

// The Settings tab shows the cap above the delete.
func TestSettingsCarriesTheCap(t *testing.T) {
	out := renderT(t, Settings(View{Name: "uploads", Base: "/v"}, "", &CapCard{Mode: "read", Value: "20 GB", Can: true, Base: "/v"}))
	i, j := strings.Index(out, "Size cap"), strings.Index(out, "Delete volume")
	if i < 0 || j < 0 || i > j {
		t.Errorf("the cap is not above the delete\n%s", out)
	}
}

// "Cap not applied yet." is a note of the read card: it must leave by itself, so the read
// card polls; an edit or a staged change is never swapped away under the person.
func TestSettingsPollsOnlyInReadMode(t *testing.T) {
	read := renderT(t, Settings(View{Name: "uploads", Base: "/v"}, "", &CapCard{Mode: "read", Value: "20 GB", Can: true, Base: "/v", Note: "Cap not applied yet."}))
	wantAll(t, "poll", read, "data-live-tab", `hx-get="/v?tab=settings"`, "Cap not applied yet.")
	for _, mode := range []string{"edit", "pending"} {
		out := renderT(t, Settings(View{Name: "uploads", Base: "/v"}, "", &CapCard{Mode: mode, Value: "20 GB", Can: true, Base: "/v"}))
		if strings.Contains(out, "data-live-tab") {
			t.Errorf("%s mode polls:\n%s", mode, out)
		}
	}
}
