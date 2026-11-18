package instance

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The allow list and the env pairs sit flat in their accordion rows.
func TestInstanceSettingsAreFlat(t *testing.T) {
	var b strings.Builder
	v := View{Name: "pg", Base: "/o/s/e/-/instances/pg"}
	s := SettingsView{Allow: []string{"a/*"}, Pairs: []Pair{{From: "dev", To: "stg"}}, Open: "allow"}
	if err := Settings(v, s).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "a/*") || !strings.Contains(b.String(), "stg") {
		t.Fatalf("no rows\n%s", b.String())
	}
	uitest.NoPanelInCard(t, b.String())
}

// The version picker starts on the running major, so nothing is a downgrade
// until someone picks one, and the button is not the red kind.
func TestMajorPickerPreselectsCurrent(t *testing.T) {
	var b strings.Builder
	v := View{Name: "pg", Base: "/b"}
	if err := majorChange(v, VersionView{Image: "postgres:17.5", Major: 17, Majors: []int{16, 18}}).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `<option value="" selected>Postgres 17 (current)</option>`) {
		t.Errorf("current major not preselected\n%s", out)
	}
	if strings.Contains(out, "btn-danger btn-sm\" data-open") {
		t.Errorf("Change version is red before a change\n%s", out)
	}
}
