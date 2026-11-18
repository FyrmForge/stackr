package slice

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The consumer rows and the extension list hold flat in their card.
func TestSliceSectionsAreFlat(t *testing.T) {
	v := full()
	v.Tab = "settings"
	v.Ext = &ExtView{Items: []ExtItem{{Name: "pgcrypto", Installed: "1.3"}}}
	out := render(t, v, Settings(v)) + render(t, v, Overview(v))
	uitest.NoPanelInCard(t, out)
}
