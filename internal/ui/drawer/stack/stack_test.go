package stack

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/access"
	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestHostAccess(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{Name: "none"}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "Elevated access") {
		t.Error("a stack with no host access shows the section")
	}
	b.Reset()
	_ = Settings(SettingsView{Name: "mon", Host: HostView{
		Tiles: access.Group([]string{"web host:/a:/b", "web privileged"}, []string{"web device:/dev/x"}),
		Approve: &access.ApproveView{
			ID:      "a",
			Action:  "/approve",
			Name:    "mon",
			Pending: []string{"web device:/dev/x"},
			Tiles:   access.Group(nil, []string{"web device:/dev/x"}),
		},
	}}).Render(context.Background(), &b)
	for _, w := range []string{
		"Elevated access", "Host folder", "Privileged", "Device", "waiting",
		`hx-post="/approve"`, `name="grant" value="web device:/dev/x" checked`,
	} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("no %s in\n%s", w, b.String())
		}
	}
	if strings.Contains(b.String(), `name="confirm"`) {
		t.Error("a device asks for the stack name")
	}
}

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
	_ = c.Releases(c.ReleasesView{Rows: []c.ReleaseRow{{Number: "3", By: "dev", Message: "fix <b>x</b>"}}}).Render(context.Background(), &b)
	_ = Settings(SettingsView{Name: "ro"}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{
		`hx-post="/o/s/-/drawer/config"`,
		`<option value="k1" selected>`,
		`value="acme/infra"`,
		"<confirm-dialog",
		"#3",
		`title="fix &lt;b&gt;x&lt;/b&gt;"`,
		`<fieldset disabled`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
}
