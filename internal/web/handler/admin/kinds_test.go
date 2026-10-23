package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

const fakeToken = "tok-never-shown-88"

// The add dialog is drawn from a kind's declared steps: a secret field is
// a password input never filled, and no answer (the form, a refusal, the
// redirect screen, the tab, the settings dialog) carries it. A redirect setup
// leaves for the provider and comes back through the one callback, which
// only the admin who began can finish.
func TestKindSetupAndCallback(t *testing.T) {
	s := webtest.NewWith(t, []service.Option{service.WithKinds(servicetest.FakeKind{})})
	ctx := context.Background()
	root := s.Session(t, s.User(t, "root@x.test", true))
	other := s.Session(t, s.User(t, "root2@x.test", true))
	const base = "/-/admin/connectors"

	if body := s.As(t, root, "GET", base+"/new", nil).Body.String(); !strings.Contains(body, `hx-get="`+base+`/new/fake"`) {
		t.Fatalf("picker lacks the fake kind:\n%s", body)
	}
	body := s.As(t, root, "GET", base+"/new/fake", nil).Body.String()
	for _, want := range []string{"Step 1 of 2: Account", "Step 2 of 2: Sign in", `type="password"`, `name="token"`, ">Next</button>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("screens lack %q\n%s", want, body)
		}
	}

	noSecret := func(what string, b string) {
		t.Helper()
		if strings.Contains(b, fakeToken) {
			t.Errorf("%s carries the secret:\n%s", what, b)
		}
	}
	// a refusal on another field: the secret is not filled back in
	rec := s.As(t, root, "POST", base+"/new/fake", url.Values{"token": {fakeToken}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Check these fields: Account.") {
		t.Fatalf("no account = %d\n%s", rec.Code, rec.Body)
	}
	noSecret("a refusal on account", rec.Body.String())
	// a refusal on the secret itself
	rec = s.As(t, root, "POST", base+"/new/fake", url.Values{"account": {"acme"}, "token": {"bad-" + fakeToken}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Fake refused the token") ||
		!strings.Contains(rec.Body.String(), `value="acme"`) {
		t.Fatalf("bad token = %d\n%s", rec.Code, rec.Body)
	}
	noSecret("a refusal on the token", rec.Body.String())

	rec = s.As(t, root, "POST", base+"/new/fake", url.Values{"account": {"acme"}, "token": {fakeToken}})
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `action="`+servicetest.FakeOutURL+`"`) || !strings.Contains(body, "Continue on Fake") {
		t.Fatalf("begin = %d\n%s", rec.Code, body)
	}
	noSecret("the redirect screen", body)
	m := regexp.MustCompile(`name="state" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no state:\n%s", body)
	}
	cb := "/-/connectors/callback?" + url.Values{"state": {m[1]}, "code": {"c0de"}}.Encode()

	if rec := s.As(t, other, "GET", cb, nil); rec.Header().Get("Location") != "/" {
		t.Errorf("another admin's callback = %d %q, want a refusal home", rec.Code, rec.Header().Get("Location"))
	}
	if cs, _ := s.Orch.ConnectorsAt(ctx, nil); len(cs) != 0 {
		t.Fatalf("another admin finished it: %v", cs)
	}
	rec = s.As(t, root, "GET", cb, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/-/admin?tab=connectors" {
		t.Fatalf("callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cs, _ := s.Orch.ConnectorsAt(ctx, nil)
	if len(cs) != 1 {
		t.Fatalf("connectors = %v", cs)
	}
	body = s.As(t, root, "GET", "/-/admin?tab=connectors", nil).Body.String()
	if !strings.Contains(body, "Fake · acme") || !strings.Contains(body, `hx-get="`+base+"/"+cs[0].ID+`/settings"`) {
		t.Fatalf("tab:\n%s", body)
	}
	noSecret("the tab", body)
	body = s.As(t, root, "GET", base+"/"+cs[0].ID+"/settings", nil).Body.String()
	if !strings.Contains(body, "<confirm-dialog open>") || !strings.Contains(body, `hx-post="`+base+"/"+cs[0].ID+`/delete"`) {
		t.Fatalf("settings:\n%s", body)
	}
	noSecret("the settings dialog", body)
}
