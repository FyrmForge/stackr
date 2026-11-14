package admin_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Adding a recipe under a name that exists is refused, and a requires that
// names no recipe is refused.
func TestAdminRecipeAddRefusals(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	f := url.Values{"name": {"pgvector"}, "packages": {"x"}, "extensions": {"x"}}
	rec := s.As(t, root, "POST", "/-/admin/extensions", f)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "a recipe pgvector already exists") {
		t.Fatalf("duplicate add = %d\n%s", rec.Code, rec.Body.String())
	}
	f = url.Values{"name": {"mine"}, "packages": {"postgresql-{major}-mine"}, "extensions": {"mine"}, "requires": {"nope"}}
	rec = s.As(t, root, "POST", "/-/admin/extensions", f)
	if b := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(b, "no recipe nope") {
		t.Fatalf("unknown requires = %d\n%s", rec.Code, b)
	}
}
