package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

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
	api.RegisterRoutes(srv, &api.Deps{Service: env.O, Access: access, DevMode: true})
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
		{"admin", "acme", 204, 200},
		{"owner", "acme", 204, 200},
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
		if got := do(t, h, cookie, "/api/v1/orgs/"+tt.path, cred{key: c.key}); got != tt.keyWant {
			t.Errorf("%s api /%s = %d, want %d", tt.who, tt.path, got, tt.keyWant)
		}
	}

	// The key path works on the web router too: one middleware.
	if got := do(t, h, cookie, "/acme", cred{key: creds["owner"].key}); got != 204 {
		t.Errorf("owner key on web = %d, want 204", got)
	}
	// Session on the API router too.
	if got := do(t, h, cookie, "/api/v1/orgs/acme", cred{session: creds["owner"].session}); got != 200 {
		t.Errorf("owner session on api = %d, want 200", got)
	}
	// The caller-only routes: any live principal, an unbound key only for an admin.
	for who, want := range map[string]int{"owner": 200, "admin": 200, "unbound": 403, "anonymous": 401, "disabled": 401} {
		if got := do(t, h, cookie, "/api/v1/orgs", cred{key: creds[who].key}); got != want {
			t.Errorf("%s GET /api/v1/orgs = %d, want %d", who, got, want)
		}
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

// A child id in the path must sit in the route's org: another org's job is
// the same 404 as a missing one, and a job outlives its tile.
func TestChildIDsStayInTheirOrg(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	access := middleware.NewAccess(env.O)
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	srv.Echo().GET("/orgs/:org/jobs/:job", ok, access.Load(), access.Require("org.read"))
	srv.Echo().GET("/orgs/:org/things/:thing", ok, access.Load(), access.Require("org.read"))

	acme, other := env.Org(t, "acme"), env.Org(t, "other")
	owner := env.User(t, "owner@x", false)
	env.Member(t, acme, owner, "owner")
	key := env.APIKey(t, owner, acme)
	mine, err := env.O.Deploy(ctx, env.Tile(t, acme).ID)
	if err != nil {
		t.Fatal(err)
	}
	theirTile := env.Tile(t, other)
	theirs, err := env.O.Deploy(ctx, theirTile.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Echo()
	for _, tt := range []struct {
		path string
		want int
	}{
		{"/orgs/acme/jobs/" + mine.ID, 204},
		{"/orgs/acme/jobs/" + theirs.ID, 404},
		{"/orgs/acme/jobs/nope", 404},
		{"/orgs/acme/things/x", 500}, // an unlisted param fails closed
	} {
		if got := do(t, h, "", tt.path, cred{key: key}); got != tt.want {
			t.Errorf("GET %s = %d, want %d", tt.path, got, tt.want)
		}
	}

	// The job keeps its org after its tile is gone.
	del, err := env.O.DeleteTile(ctx, theirTile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if org, err := env.O.OrgOf(ctx, "job", del.ID); err != nil || org != other {
		t.Errorf("OrgOf(deleted tile's job) = %q, %v; want %q", org, err, other)
	}
}
