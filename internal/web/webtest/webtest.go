// Package webtest is the site under test: a server with every web route,
// an org "acme" with shop/dev/api seeded, and its owner's browser session.
package webtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
)

type Site struct {
	*servicetest.Env
	Org     string
	Tile    servicetest.Tile
	h       http.Handler
	session string
}

func New(t *testing.T) *Site {
	t.Helper()
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Service: env.O, Access: middleware.NewAccess(env.O), DevMode: true})
	org := env.Org(t, "acme")
	owner := env.User(t, "owner@acme.test", false)
	env.Member(t, org, owner, "owner")
	return &Site{Env: env, Org: org, Tile: env.Tile(t, org), h: srv.Echo(), session: env.Session(t, owner)}
}

// Do sends a request as the owner, CSRF token included; form, if any, is
// the urlencoded body.
func (s *Site) Do(t *testing.T, method, path string, form url.Values) *httptest.ResponseRecorder {
	return s.DoCtx(context.Background(), t, method, path, form)
}

// DoCtx is Do under ctx, for streams: cancel it to end the response.
func (s *Site) DoCtx(ctx context.Context, t *testing.T, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode())).WithContext(ctx)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", "tok")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
	req.AddCookie(&http.Cookie{Name: s.O.Sessions().CookieName(), Value: s.session})
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// DoNoCSRF is Do without the CSRF token.
func (s *Site) DoNoCSRF(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: s.O.Sessions().CookieName(), Value: s.session})
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}
