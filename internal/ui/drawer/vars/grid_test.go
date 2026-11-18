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

// A held change shows its old and new, what it starts, three buttons.
func TestPendingFooter(t *testing.T) {
	g := envGrid()
	g.Pending = &Pending{CellRef: CellRef{Scope: "env:1", Group: "gate", Name: "password"}, Label: "gate.password", Old: "unset", New: "a new value",
		Field: "secret.gate.password", Value: "pw", Impact: []string{"Starts bazarr, radarr, sonarr."}, Starts: true, Tiles: 3}
	got := page(t, g)
	for _, w := range []string{"1 change not saved", "gate.password", "a new value", "Starts bazarr, radarr, sonarr.", "Apply and start 3 tiles", "Apply, start later", ">Cancel<", `name="secret.gate.password"`, `name="restart" value="later"`} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	g.Pending.Tiles, g.Pending.Impact = 0, nil
	if got := page(t, g); !strings.Contains(got, ">Apply<") || strings.Contains(got, "later") {
		t.Errorf("a change nobody reads has more than Apply:\n%s", got)
	}
}
