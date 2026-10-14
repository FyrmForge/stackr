package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func tokenReq(host, key, scope string) *http.Request {
	r := httptest.NewRequest("GET", "/token?service=stackr-registry&scope="+scope, nil)
	r.Host = host
	if key != "" {
		r.SetBasicAuth("x", key)
	}
	return r
}

func TestRegistryToken(t *testing.T) {
	e := servicetest.New(t, func(c *service.Config) { c.PanelDomain = "panel.test" })
	ctx := context.Background()
	org := e.Org(t, "acme")
	e.Tile(t, org) // acme/shop/api
	dev := e.User(t, "dev@x.test", false)
	e.Member(t, org, dev, "member")
	viewer := e.User(t, "view@x.test", false)
	e.Member(t, org, viewer, "viewer")
	devKey, viewKey := e.APIKey(t, dev, org), e.APIKey(t, viewer, org)

	srv := echo.New()
	srv.Pre(e.Orch.RegistryMiddleware())
	srv.GET("/token", func(c echo.Context) error { return c.String(200, "panel") })
	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	granted := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var body struct{ Token string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("token = %d %s", w.Code, w.Body)
		}
		return body.Token
	}

	// Other hosts reach the panel's own routes.
	if w := do(tokenReq("panel.test", "", "")); w.Body.String() != "panel" {
		t.Fatalf("panel host = %q", w.Body)
	}
	if w := do(tokenReq("registry.panel.test", "", "")); w.Code != 401 {
		t.Fatalf("no credentials = %d", w.Code)
	}
	if w := do(tokenReq("registry.panel.test", "nope", "")); w.Code != 401 {
		t.Fatalf("bad key = %d", w.Code)
	}

	// A member pushes; a viewer only pulls; both are audited.
	granted(do(tokenReq("registry.panel.test", devKey, "repository:acme/shop/api:pull,push")))
	granted(do(tokenReq("registry.panel.test", viewKey, "repository:acme/shop/api:pull,push")))
	rows, err := e.Store.RegistryTokens.ListBySubject(ctx, dev, 5)
	if err != nil || len(rows) != 1 || rows[0].Scope != "acme/shop/api:pull,push" || rows[0].Kind != "user" {
		t.Fatalf("dev row = %+v, %v", rows, err)
	}
	rows, _ = e.Store.RegistryTokens.ListBySubject(ctx, viewer, 5)
	if len(rows) != 1 || rows[0].Scope != "acme/shop/api:pull" {
		t.Fatalf("viewer row = %+v", rows)
	}
	evs, err := e.Orch.SecurityLog(ctx, service.SecurityFilter{Action: "registry.token"})
	if err != nil || len(evs) != 2 || !strings.Contains(evs[0].Detail, "acme/shop/api") {
		t.Fatalf("security log = %+v, %v", evs, err)
	}

	// Another org's image is not granted.
	granted(do(tokenReq("registry.panel.test", devKey, "repository:other/shop/api:pull")))
	rows, _ = e.Store.RegistryTokens.ListBySubject(ctx, dev, 5)
	if rows[0].Scope != "" {
		t.Fatalf("other org scope = %q", rows[0].Scope)
	}
}

// `stackr registry login` hands docker a short-lived token minted for the
// caller: it works at the registry front for the caller's orgs only, and a
// tampered or API-key-shaped value does not.
func TestRegistryLoginToken(t *testing.T) {
	e := servicetest.New(t, func(c *service.Config) { c.PanelDomain = "panel.test" })
	ctx := context.Background()
	org := e.Org(t, "acme")
	e.Tile(t, org)
	dev := e.User(t, "dev@x.test", false)
	e.Member(t, org, dev, "member")
	devKey := e.APIKey(t, dev, org)
	p, err := e.Orch.KeyPrincipal(ctx, devKey)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Orch.RegistryLogin(ctx, p)
	if err != nil || out.Token == devKey || strings.Contains(out.Token, devKey) || time.Until(out.ExpiresAt) > service.RegistryLoginTTL {
		t.Fatalf("login = %+v, %v", out, err)
	}
	srv := echo.New()
	srv.Pre(e.Orch.RegistryMiddleware())
	do := func(key, scope string) int {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, tokenReq("registry.panel.test", key, scope))
		return w.Code
	}
	if c := do(out.Token, "repository:acme/shop/api:pull,push"); c != 200 {
		t.Fatalf("minted token = %d", c)
	}
	rows, _ := e.Store.RegistryTokens.ListBySubject(ctx, dev, 5)
	if len(rows) != 1 || rows[0].Scope != "acme/shop/api:pull,push" {
		t.Fatalf("row = %+v", rows)
	}
	if c := do(out.Token+"x", ""); c != 401 {
		t.Errorf("tampered = %d", c)
	}
	if c := do("stackr-rl.e30.AAAA", ""); c != 401 {
		t.Errorf("forged = %d", c)
	}
	// the login token is not an API key
	if _, err := e.Orch.KeyPrincipal(ctx, out.Token); err == nil {
		t.Error("the registry login token authenticated as an API key")
	}
}

// The login token freezes nothing: a demotion and a revoked key are felt at
// the next /token, not 12 hours later.
func TestRegistryLoginTokenFollowsLiveRights(t *testing.T) {
	e := servicetest.New(t, func(c *service.Config) { c.PanelDomain = "panel.test" })
	ctx := context.Background()
	org := e.Org(t, "acme")
	e.Tile(t, org)
	dev := e.User(t, "dev@x.test", false)
	e.Member(t, org, dev, "member")
	p, err := e.Orch.KeyPrincipal(ctx, e.APIKey(t, dev, org))
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Orch.RegistryLogin(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	srv := echo.New()
	srv.Pre(e.Orch.RegistryMiddleware())
	do := func() int {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, tokenReq("registry.panel.test", out.Token, "repository:acme/shop/api:pull,push"))
		return w.Code
	}
	if c := do(); c != 200 {
		t.Fatalf("fresh = %d", c)
	}
	m, err := e.Store.OrgMembers.GetByOrgUser(ctx, org, dev)
	if err != nil {
		t.Fatal(err)
	}
	m.Role = "viewer"
	if err := e.Store.OrgMembers.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	if c := do(); c != 200 {
		t.Fatalf("demoted = %d", c)
	}
	rows, _ := e.Store.RegistryTokens.ListBySubject(ctx, dev, 10)
	var scopes []string
	for _, r := range rows {
		scopes = append(scopes, r.Scope)
	}
	if !slices.Contains(scopes, "acme/shop/api:pull") {
		t.Fatalf("a demoted member still got push: %v", scopes)
	}
	if err := e.Store.APIKeys.Delete(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	if c := do(); c != 401 {
		t.Fatalf("revoked key = %d", c)
	}
}
