package connector

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestTabs(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{
		Base:       "/o/-/connectors/k1",
		Name:       "gh",
		Host:       "github.com",
		InstallURL: "https://github.com/apps/stackr-x/installations/new",
		Delete:     c.ConfirmView{Action: "/o/-/connectors/k1/delete"},
	}).Render(context.Background(), &b)
	_ = Repos(ReposView{Check: "/o/-/connectors/k1/check", InstallURL: "https://github.com/apps/stackr-x/installations/new"}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{
		"github.com",
		`hx-post="/o/-/connectors/k1/rename"`,
		`href="https://github.com/apps/stackr-x/installations/new"`,
		"Manage installation",
		`hx-post="/o/-/connectors/k1/check"`,
		"Check installation",
		"<confirm-dialog",
		`id="tab-repos"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
}

func TestCheckStates(t *testing.T) {
	render := func(v ReposView) string {
		var b strings.Builder
		_ = Repos(v).Render(context.Background(), &b)
		return b.String()
	}
	install := "https://github.com/apps/stackr-x/installations/new"
	if got := render(ReposView{Check: "/c", InstallURL: install, Checked: true}); !strings.Contains(got, "Install it on GitHub") || !strings.Contains(got, "check again") {
		t.Errorf("not installed:\n%s", got)
	}
	got := render(ReposView{Check: "/c", InstallURL: install, Checked: true, Repos: []string{"acme/api", "acme/web"}})
	for _, w := range []string{"installed on 2 repositories", "acme/api", "acme/web", "Change which repositories"} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	if got := render(ReposView{Check: "/c", InstallURL: install, Checked: true, Repos: []string{"acme/api"}}); !strings.Contains(got, "installed on 1 repository.") {
		t.Errorf("singular:\n%s", got)
	}
	if got := render(ReposView{Check: "/c"}); !strings.Contains(got, "did not finish") || strings.Contains(got, "Check installation") {
		t.Errorf("pending:\n%s", got)
	}
}

func TestPendingHasNoInstallLink(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{Name: "GitHub (connecting…)", Host: "github.com"}).Render(context.Background(), &b)
	got := b.String()
	if strings.Contains(got, "Manage installation") || !strings.Contains(got, "setup not finished") {
		t.Errorf("pending connector rendered wrong:\n%s", got)
	}
}
