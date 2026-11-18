package tile

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The settings accordion rows (variables, domains, volumes, image) and the
// access tab's sections hold their rows flat: no panel box inside a card.
func TestTileSettingsAreFlat(t *testing.T) {
	v := View{Node: "t1", Name: "api", Kind: "service", Base: "/o/s/e/-/tiles/api", Tab: "settings", Status: "running"}
	var b strings.Builder
	err := Settings(v, SettingsView{
		Name:    "api",
		Kind:    "service",
		Vals:    map[string]string{"port": "80", "image": "nginx:1", "volumes": ""},
		Open:    "variables",
		Volumes: []VolumeRow{{ID: "v1", Name: "uploads", Drawer: "/d"}},
		Env:     EnvView{Rows: []EnvRow{{Name: "A", Value: "1"}}, Text: "A=1"},
		Domains: &DomainsView{Admin: true, Rows: []DomainRow{{ID: "d1", Host: "api.acme.dev", Port: "80"}}},
		Image:   &ImageView{Ref: "nginx:1"},
	}).Render(t.Context(), &b)
	if err != nil {
		t.Fatal(err)
	}
	if err := Access(v, AccessView{Entries: []AccessEntry{{Slice: "main"}}}).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"uploads", "api.acme.dev", "nginx:1", "main"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %s rendered", want)
		}
	}
	uitest.NoPanelInCard(t, b.String())
}
