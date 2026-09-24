package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
)

// Both routers, one Access, a seeded world.
func setup(t *testing.T) (*servicetest.Env, http.Handler) {
	t.Helper()
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	access := middleware.NewAccess(env.O)
	web.RegisterStaticPages(srv)
	api.RegisterRoutes(srv, &api.Deps{Service: env.O, Access: access})
	web.RegisterRoutes(srv, &web.Deps{Service: env.O, Access: access, DevMode: true})
	return env, srv.Echo()
}

type cred struct{ session, key string }

func do(t *testing.T, h http.Handler, cookie string, path string, c cred) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c.session != "" {
		req.AddCookie(&http.Cookie{Name: cookie, Value: c.session})
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAccess(t *testing.T) {
	env, h := setup(t)
	ctx := context.Background()
	cookie := env.O.Sessions().CookieName()

	acme := env.Org(t, "acme")
	other := env.Org(t, "other")
	admin := env.User(t, "admin@x", true)
	owner := env.User(t, "owner@x", false)
	stranger := env.User(t, "stranger@x", false)
	disabled := env.User(t, "disabled@x", false)
	removed := env.User(t, "removed@x", false)
	drifted := env.User(t, "drifted@x", false)
	for _, u := range []string{owner, disabled, removed, drifted} {
		env.Member(t, acme, u, "owner")
	}
	env.Member(t, other, stranger, "owner")

	creds := map[string]cred{
		"admin":      {session: env.Session(t, admin), key: env.APIKey(t, admin, "")},
		"owner":      {session: env.Session(t, owner), key: env.APIKey(t, owner, acme)},
		"non-member": {session: env.Session(t, stranger), key: env.APIKey(t, stranger, other)},
		"disabled":   {session: env.Session(t, disabled), key: env.APIKey(t, disabled, acme)},
		"removed":    {session: env.Session(t, removed), key: env.APIKey(t, removed, acme)},
		"drifted":    {session: env.Session(t, drifted), key: env.APIKey(t, drifted, acme)},
		"unbound":    {key: env.APIKey(t, owner, "")},
		"anonymous":  {},
	}

	// Revoke through the verbs: both close sessions and keys (B16).
	if err := env.O.DisableUser(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	if err := env.O.RemoveMember(ctx, acme, removed); err != nil {
		t.Fatal(err)
	}
	// A membership gone without the revoke verb: the key is still there,
	// but its abilities are the user's live ones.
	m, err := env.Store.OrgMembers.GetByOrgUser(ctx, acme, drifted)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store.OrgMembers.Delete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		who      string
		path     string
		sessWant int // web, with the session cookie
		keyWant  int // api, with the bearer key
	}{
		{"admin", "acme", 204, 204},
		{"owner", "acme", 204, 204},
		{"owner", "nope", 404, 404},
		{"owner", "other", 404, 404},
		{"non-member", "acme", 404, 404},
		{"disabled", "acme", 401, 401},
		{"removed", "acme", 401, 401},
		{"drifted", "acme", 404, 404},
		{"unbound", "acme", 0, 403},
		{"anonymous", "acme", 401, 401},
	}
	for _, tt := range tests {
		c := creds[tt.who]
		if tt.sessWant != 0 {
			if got := do(t, h, cookie, "/"+tt.path, cred{session: c.session}); got != tt.sessWant {
				t.Errorf("%s web /%s = %d, want %d", tt.who, tt.path, got, tt.sessWant)
			}
		}
		if got := do(t, h, cookie, "/api/"+tt.path, cred{key: c.key}); got != tt.keyWant {
			t.Errorf("%s api /%s = %d, want %d", tt.who, tt.path, got, tt.keyWant)
		}
	}

	// The key path works on the web router too: one middleware.
	if got := do(t, h, cookie, "/acme", cred{key: creds["owner"].key}); got != 204 {
		t.Errorf("owner key on web = %d, want 204", got)
	}
	// Session on the API router too.
	if got := do(t, h, cookie, "/api/acme", cred{session: creds["owner"].session}); got != 204 {
		t.Errorf("owner session on api = %d, want 204", got)
	}
}

// Static routes win over /:org.
func TestFixedRoutesWin(t *testing.T) {
	_, h := setup(t)
	for _, path := range []string{"/", "/login", "/about", "/api/health"} {
		if got := do(t, h, "", path, cred{}); got != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, got)
		}
	}
}
