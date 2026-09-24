package connector

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestTabs(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{Base: "/o/-/connectors/k1", Name: "gh", Host: "github.com", Delete: c.ConfirmView{Action: "/o/-/connectors/k1/delete"}}).Render(context.Background(), &b)
	_ = Repos().Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{"github.com", `hx-post="/o/-/connectors/k1/rename"`, "<confirm-dialog", `id="tab-repos"`} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
}
