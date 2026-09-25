package stack

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestTabs(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{
		Base:       "/o/s/-/drawer",
		Name:       "shop",
		Connectors: []Option{{"k1", "gh"}},
		Connector:  "k1",
		Repo:       "acme/infra",
		Delete:     c.ConfirmView{Word: "shop", Kept: []string{"x"}, Action: "/o/s/-/drawer/delete"},
	}).Render(context.Background(), &b)
	_ = c.Releases(c.ReleasesView{Rows: []c.ReleaseRow{{Number: "3", By: "dev"}}}).Render(context.Background(), &b)
	_ = Settings(SettingsView{Name: "ro"}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{
		`hx-post="/o/s/-/drawer/config"`,
		`<option value="k1" selected>`,
		`value="acme/infra"`,
		"<confirm-dialog",
		"#3",
		`<fieldset disabled`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
}
