package service_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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

// A node's cert gets the images of the envs pinned to it and nothing else;
// a revoked cert gets nothing.
func TestRegistryNodeCert(t *testing.T) {
	e := servicetest.New(t, func(c *service.Config) { c.PanelDomain = "panel.test" })
	ctx := context.Background()
	org := e.Org(t, "acme")
	tl := e.Tile(t, org)
	env, err := e.Store.Environments.Get(ctx, tl.Env)
	if err != nil || env.NodeID == "" {
		t.Fatalf("env node = %+v, %v", env, err)
	}

	ca, err := clusterca.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := clusterca.SaveCA(e.Config.DataDir, ca); err != nil {
		t.Fatal(err)
	}
	issue := func(node string) (*x509.Certificate, string) {
		key, _ := clusterca.NewKey()
		csr, _ := clusterca.NewCSR(key)
		is, err := ca.IssueNode(csr, node, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		c, err := clusterca.ParseCertPEM(is.CertPEM)
		if err != nil {
			t.Fatal(err)
		}
		return c, is.Serial
	}
	req := func(c *x509.Certificate) *httptest.ResponseRecorder {
		r := tokenReq("registry.panel.test", "", "repository:acme/shop/api:pull,push")
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}}
		w := httptest.NewRecorder()
		e.Orch.RegistryHandler().ServeHTTP(w, r)
		return w
	}
	scope := func(node string) string {
		rows, _ := e.Store.RegistryTokens.ListBySubject(ctx, node, 5)
		if len(rows) == 0 {
			return "none"
		}
		return rows[0].Scope
	}

	c, serial := issue(env.NodeID)
	if w := req(c); w.Code != 200 || scope(env.NodeID) != "acme/shop/api:pull" {
		t.Fatalf("pinned node: %d %q", w.Code, scope(env.NodeID))
	}
	if w := req(func() *x509.Certificate { c, _ := issue("not-a-node"); return c }()); w.Code != 401 {
		t.Fatalf("unknown node = %d", w.Code)
	}
	if err := e.Store.NodeCerts.Create(ctx, store.NodeCert{Serial: serial, NodeID: env.NodeID, Role: "worker", NotAfter: c.NotAfter, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.NodeCerts.Revoke(ctx, serial, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := req(c); w.Code != 401 {
		t.Fatalf("revoked = %d", w.Code)
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
