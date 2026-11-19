package vars

import (
	"context"
	"strings"
	"testing"
)

func envGrid() *Grid {
	return &Grid{
		Action: "/o/s/e/-/vars",
		Cols:   []Col{{Token: "env:1", Label: "prod"}},
		Groups: []Group{{Name: "gate", Rows: []Row{
			{Name: "password", Secret: true, Cells: []Cell{{Waits: []string{"bazarr", "radarr", "sonarr"}}}},
			{Name: "token", Secret: true, Cells: []Cell{{Set: true, Secret: true}}},
		}}},
	}
}

func page(t *testing.T, g *Grid) string {
	t.Helper()
	var b strings.Builder
	if err := Editor(View{Grid: g}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The env view is flat rows: a declared secret is unset with Set and the
// tiles that wait; a stored one is a mask with Show; no value is in the page.
func TestEnvViewRows(t *testing.T) {
	got := page(t, envGrid())
	for _, w := range []string{"divide-y", "unset", "bazarr, radarr and sonarr wait for it.", `aria-label="Set gate.password"`, `aria-label="Show gate.token"`, `hx-post="/o/s/e/-/vars/reveal"`, "••••••••", `data-poll-guard="hold dirty region"`} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	if strings.Contains(got, "<table") {
		t.Error("one column drew a table")
	}
}

// A revealed secret shows its value, a way back and the 30 s hide; the page
// holds the poll off meanwhile.
func TestShownSecret(t *testing.T) {
	g := envGrid()
	g.Shown = &Shown{CellRef: CellRef{Scope: "env:1", Group: "gate", Name: "token"}, Value: "abc123"}
	got := page(t, g)
	for _, w := range []string{"abc123", ">Hide<", "Shown to you. Logged. Hides in 30 s.", `hx-trigger="load delay:30s"`, "data-hold"} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	if strings.Count(got, "abc123") != 1 {
		t.Error("the value is in the page more than once")
	}
}

// Held changes show their old and new, what they start, three buttons, and
// post themselves back so no click loses them.
func TestPendingFooter(t *testing.T) {
	g := envGrid()
	g.Held = &Held{Changes: []Pending{
		{CellRef: CellRef{Scope: "env:1", Group: "gate", Name: "password"}, Label: "gate.password", Old: "unset", New: "a new value", Field: "secret.gate.password", Value: "pw"},
		{CellRef: CellRef{Scope: "env:1", Group: "gate", Name: "user"}, Label: "gate.user", Old: "admin", New: "root", Field: "param.gate.user", Value: "root"},
	}, Impact: []string{"Starts bazarr, radarr, sonarr."}, Starts: true, Tiles: 3}
	got := page(t, g)
	for _, w := range []string{"2 changes not saved", "gate.password", "a new value", "gate.user", "Starts bazarr, radarr, sonarr.", "Apply and start 3 tiles", "Apply, start later", ">Cancel<", `name="held.secret.gate.password"`, `name="held.param.gate.user"`, `name="held_scope" value="env:1"`, `id="vars-held"`, `name="restart" value="later"`} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	// every action that redraws the editor posts the held changes
	if n := strings.Count(got, `hx-include="#vars-held, .vars-edit"`); n < 3 {
		t.Errorf("only %d actions post the held changes:\n%s", n, got)
	}
	g.Held.Tiles, g.Held.Impact = 0, nil
	if got := page(t, g); !strings.Contains(got, ">Apply<") || strings.Contains(got, `value="later"`) {
		t.Errorf("a change nobody reads has more than Apply:\n%s", got)
	}
	// a draft (input still open) is listed but its value is not held
	g.Held.Changes = []Pending{{CellRef: CellRef{Scope: "env:1", Group: "gate", Name: "password"}, Label: "gate.password", Old: "unset", New: "a new value", Field: "secret.gate.password", Draft: true}}
	g.Edit = &CellRef{Scope: "env:1", Group: "gate", Name: "password"}
	if got := page(t, g); !strings.Contains(got, "1 change not saved") || strings.Contains(got, `name="held.secret.gate.password"`) || strings.Contains(got, ">Review<") {
		t.Errorf("draft:\n%s", got)
	}
}

// A revealed or typed secret is on the page, so the editor stays out of
// htmx's history snapshot (localStorage).
func TestEditorSkipsHistory(t *testing.T) {
	if got := page(t, envGrid()); !strings.Contains(got, `id="vars-editor" class="space-y-4" hx-history="false"`) {
		t.Errorf("editor root lacks hx-history=false:\n%s", got)
	}
}

// The live refresh swaps the grid only: the save note and the form error sit
// outside it, so a poll cannot wipe them, and #vars-editor stays put for a
// click's swap.
func TestPollSwapsGridOnly(t *testing.T) {
	var b strings.Builder
	if err := Editor(View{Grid: envGrid(), Note: "Saved.", Error: "Nope."}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, w := range []string{`hx-select="#vars-grid"`, `hx-target="#vars-grid"`} {
		if !strings.Contains(got, w) {
			t.Errorf("poll lacks %s", w)
		}
	}
	g := strings.Index(got, `id="vars-grid"`)
	if g < 0 || strings.Index(got, "Saved.") > g || strings.Index(got, "Nope.") > g {
		t.Errorf("note or error is inside the polled grid:\n%s", got)
	}
}

// Copy: the tiles a save starts read as a list with "and", and the hints do
// not claim a save always redeploys (it can be applied later).
func TestSaveCopy(t *testing.T) {
	g := envGrid()
	g.Edit = &CellRef{Scope: "env:1", Group: "gate", Name: "password"}
	got := page(t, g)
	for _, w := range []string{"Saving starts bazarr, radarr and sonarr.", "can redeploy the running tiles"} {
		if !strings.Contains(got, w) {
			t.Errorf("no %q in\n%s", w, got)
		}
	}
	if strings.Contains(got, " redeploys the running") {
		t.Error("a hint says a save always redeploys")
	}
}
