package env

import (
	"strings"
	"testing"

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
