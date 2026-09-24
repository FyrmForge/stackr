package dialog

import (
	"context"
	"strings"
	"testing"
)

func TestCreateLevel(t *testing.T) {
	var b strings.Builder
	_ = CreateLevel(CreateLevelView{Kind: "env", Action: "/o/s/-/new-env", Name: "prod", Errors: map[string]string{"name": "taken"}}).Render(context.Background(), &b)
	_ = CreateLevel(CreateLevelView{Kind: "stack", Action: "/o/-/new-stack"}).Render(context.Background(), &b)
	_ = InstallConnector(InstallConnectorView{Action: "https://github.com/settings/apps/new", Manifest: `{"a":1}`}).Render(context.Background(), &b)
	got := b.String()
	for _, w := range []string{`id="create-env"`, `hx-target="#create-env"`, "taken", `name="from"`, `id="create-stack"`,
		`action="https://github.com/settings/apps/new"`, `name="manifest"`} {
		if !strings.Contains(got, w) {
			t.Errorf("no %s in\n%s", w, got)
		}
	}
	if strings.Count(got, `name="from"`) != 2 {
		t.Error("the stack form asks where releases come from")
	}
	_ = DeleteOrg("acme", "/a", "#t") // a type-the-name confirm must say what is kept
}
