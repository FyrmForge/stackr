package canvas_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	comp "github.com/FyrmForge/stackr/internal/ui/components"
)

// Invite links follow the panel_domain setting once the panel moves, keeping
// the scheme and port BASE_URL has: the old host's vhost is gone.
func TestInviteLinkFollowsPanelDomain(t *testing.T) {
	old := comp.BaseURL
	comp.BaseURL = "https://stkr.a.example.com:8443"
	t.Cleanup(func() { comp.BaseURL = old })
	b := newBrowser(t, "owner")
	if err := b.env.Orch.SetSetting(context.Background(), "panel_domain", "stkr.b.example.com"); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"email": {"new@acme.test"}, "role": {"owner"}}
	body := b.do(t, "POST", "/acme/-/drawer/invite", form, true).Body.String()
	if !strings.Contains(body, "Invite link: https://stkr.b.example.com:8443/invite/") || strings.Contains(body, "stkr.a.example.com") {
		t.Errorf("invite note:\n%s", body)
	}
	list := b.do(t, "GET", "/acme/-/drawer?tab=members", nil, true).Body.String()
	if !strings.Contains(list, `data-copy="https://stkr.b.example.com:8443/invite/`) || strings.Contains(list, "stkr.a.example.com") {
		t.Errorf("pending invite row:\n%s", list)
	}
}
