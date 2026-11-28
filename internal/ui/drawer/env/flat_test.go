package env

import (
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The Volumes row holds its list flat: no panel box inside the accordion.
func TestVolumesAreFlat(t *testing.T) {
	var b strings.Builder
	v := SettingsView{Name: "dev", Open: "volumes", Volumes: VolumesView{Rows: []VolumeRow{{Name: "data", Size: "1G"}}}}
	if err := Settings(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "data") {
		t.Fatal("no volume row")
	}
	uitest.NoPanelInCard(t, b.String())
}

// Remove is a typed confirm: the volume's name and the Back up first tick.
func TestVolumeRemoveIsATypedConfirm(t *testing.T) {
	var b strings.Builder
	rm := c.ConfirmView{Button: "Remove", Title: "Delete data?", Warning: "Its data is deleted at once.", Kept: []string{"backups"}, Word: "data", Action: "/rm", Target: "#x"}
	v := SettingsView{Name: "dev", Open: "volumes", Volumes: VolumesView{Add: "/add", Rows: []VolumeRow{{Name: "data", Size: "1G", Remove: rm}}}}
	if err := Settings(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`word="data"`, `hx-post="/rm"`, "Removing a volume deletes its data at once."} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("no %q", w)
		}
	}
}
