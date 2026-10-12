package api_test

import "testing"

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
