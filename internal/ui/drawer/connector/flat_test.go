package connector

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

// The connector tabs hold their name row and repo list flat in the card.
func TestConnectorSectionsAreFlat(t *testing.T) {
	var b strings.Builder
	url := "https://example.com/apps/x/installations/new"
	_ = Settings(SettingsView{Base: "/c", Name: "gh", InstallURL: url}).Render(t.Context(), &b)
	_ = Repos(ReposView{Check: "/c/check", InstallURL: url, Checked: true, Repos: []string{"a/b"}}).Render(t.Context(), &b)
	if !strings.Contains(b.String(), "a/b") {
		t.Fatal("no repo row")
	}
	uitest.NoPanelInCard(t, b.String())
}
