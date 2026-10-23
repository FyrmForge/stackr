package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A writer (member) adds the org's git connector from the canvas's
// "+ Create connector", as they always could; a DNS kind stays an owner's.
func TestWriterAddsGitConnector(t *testing.T) {
	s := webtest.New(t)
	m := s.User(t, "member@acme.test", false)
	s.Member(t, s.Org, m, "member")
	member := s.Session(t, m)

	if body := s.As(t, member, "GET", "/acme", nil).Body.String(); !strings.Contains(body, `/acme/-/new-connector`) {
		t.Errorf("the canvas offers a writer no create button")
	}
	rec := s.As(t, member, "GET", "/acme/-/new-connector", nil)
	if b := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(b, `name="owner"`) || !strings.Contains(b, "<confirm-dialog open>") {
		t.Fatalf("writer's dialog = %d\n%s", rec.Code, b)
	}
	rec = s.As(t, member, "POST", "/acme/-/drawer/connectors/new/github", url.Values{"owner": {""}, "return": {"/acme"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="connector-out"`) {
		t.Fatalf("writer's begin = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.As(t, member, "POST", "/acme/-/drawer/connectors/new/namecheap", url.Values{"user": {"jo"}, "key": {"k"}, "client_ip": {"203.0.113.7"}}); rec.Code != http.StatusForbidden {
		t.Errorf("writer's DNS add = %d, want 403", rec.Code)
	}
}

// Removing a server git connector a stack is bound to is refused, the
// stack linked to its drawer, into the dialog it was asked from.
func TestRemoveRefusalLinksUsages(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	srv := s.ServerConnector(t, "main", "sec")
	if _, err := s.Orch.ShareConnector(ctx, srv, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.SetConfigRepo(ctx, s.Tile.Stack, srv, "https://github.com/acme/shop", "main", ""); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	rec := s.As(t, root, "POST", "/-/admin/connectors/"+srv+"/delete", nil)
	if b := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(b, `href="/acme/shop?drawer=stack:`+s.Tile.Stack+`"`) ||
		rec.Header().Get("HX-Retarget") != "#connector-dialog-body" {
		t.Errorf("remove in use = %d %q\n%s", rec.Code, rec.Header().Get("HX-Retarget"), b)
	}
}
