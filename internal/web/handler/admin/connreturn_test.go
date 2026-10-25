package admin_test

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A wizard reopened after the server setup is done still gets the browser
// back from the provider's install page: the install link carries where
// it was opened, and the provider's return lands there. A return with no
// state (a change made at the provider) goes to Admin's Connectors tab;
// one whose state was tampered with too. The callback path Apps were
// registered with still reaches the core.
func TestInstallReturnsWhereItBegan(t *testing.T) {
	s := webtest.New(t)
	srv := s.ServerConnector(t, "main", "sec")
	root := s.Session(t, s.User(t, "root@x.test", true))

	body := s.As(t, root, "GET", "/-/setup/server/ui/git", nil).Body.String()
	m := regexp.MustCompile(`href="(https://github\.com/apps/stackr-main/installations/new\?state=[^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no install link with a state on the reopened step:\n%s", body)
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	back := "/settings/github/installed/" + srv
	if got := s.As(t, root, "GET", back+"?state="+url.QueryEscape(state), nil).Header().Get("Location"); got != "/-/setup/server/ui/git" {
		t.Errorf("return from the wizard's install = %q", got)
	}
	if got := s.As(t, root, "GET", back, nil).Header().Get("Location"); got != "/-/admin/connectors" {
		t.Errorf("return with no state = %q", got)
	}
	bad := strings.Replace(state, ".", ".0", 1)
	if got := s.As(t, root, "GET", back+"?state="+url.QueryEscape(bad), nil).Header().Get("Location"); got != "/-/admin/connectors" {
		t.Errorf("return with a forged state = %q", got)
	}
	if got := s.As(t, root, "GET", "/settings/github/callback?state=nope.nope&code=c", nil).Header().Get("Location"); got != "/" {
		t.Errorf("a stray callback on the registered path = %q, want a refusal home", got)
	}
}
