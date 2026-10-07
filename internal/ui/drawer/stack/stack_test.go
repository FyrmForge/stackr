package stack

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func TestHostAccess(t *testing.T) {
	var b strings.Builder
	_ = Settings(SettingsView{Name: "none"}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "Host access") {
		t.Error("a stack with no host access shows the section")
	}
	b.Reset()
	_ = Settings(SettingsView{Name: "mon", Host: HostView{
		Lines:      []string{"host:/a:/b"},
		Privileged: true,
		Pending:    []string{"device:/dev/x"},
		Approve:    c.ConfirmView{Button: "Approve", Title: "Approve?", Action: "/approve", Primary: true},
	}}).Render(context.Background(), &b)
	for _, w := range []string{"Host access", "host:/a:/b", "privileged", "waiting: device:/dev/x", `hx-post="/approve"`} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("no %s in\n%s", w, b.String())
		}
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
