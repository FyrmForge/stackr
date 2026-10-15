package api_test

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The join calls: admin only to mint, confirm and reject; the public two
// refuse plain http, answer an unknown token with a 404, and stop a flood
// from one address.
func TestJoinAPI(t *testing.T) {
	w := newWorld(t)
	for _, p := range []struct{ method, path, body string }{
		{"POST", "/admin/join-tokens", `{"node_name":"w1"}`},
		{"GET", "/admin/join-requests", ""},
		{"POST", "/admin/join-requests/x/confirm", ""},
		{"POST", "/admin/join-requests/x/reject", ""},
	} {
		if code, _ := w.do(t, "", p.method, p.path, p.body); code != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, code)
		}
		if code, _ := w.do(t, w.owner, p.method, p.path, p.body); code != 403 {
			t.Errorf("%s %s owner = %d, want 403", p.method, p.path, code)
		}
	}
	if code, _ := w.do(t, w.admin, "GET", "/admin/join-requests", ""); code != 200 {
		t.Errorf("admin list = %d", code)
	}
	if code, _ := w.do(t, "", "POST", "/join/status", `{"token":"stkjoin_nope"}`); code != 404 {
		t.Errorf("unknown token status = %d, want 404", code)
	}
	// plain http is refused before the token is looked at
	if code, _ := w.do(t, "", "POST", "/join", `{"token":"stkjoin_nope","csr":"x","wg_public_key":"x"}`); code != 403 {
		t.Errorf("join over http = %d, want 403", code)
	}
	limited := false
	for range 30 {
		if code, _ := w.do(t, "", "POST", "/join/status", `{"token":"stkjoin_nope"}`); code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("30 join calls from one address were never limited")
	}
}

// The one-line join's script and download sit outside the API and the site:
// a refusal is plain text (never a login redirect sh would run), never
// cached, and a flood from one address is stopped. This panel has no
// trusted certificate, so it serves neither.
func TestJoinScriptRoutes(t *testing.T) {
	w := newWorld(t)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	for _, p := range []string{"/join/stkjoin_nope", "/join/stkjoin_nope/stackr-install"} {
		rec := get(p)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "two-step join command") ||
			rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Location") != "" {
			t.Errorf("GET %s = %d %q %v", p, rec.Code, rec.Body, rec.Header())
		}
	}
	limited := false
	for range 30 {
		if get("/join/stkjoin_nope").Code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("30 script fetches from one address were never limited")
	}
}
