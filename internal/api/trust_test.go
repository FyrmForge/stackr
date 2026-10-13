package api_test

import (
	"strings"
	"testing"
)

// The trust routes are for server admins: anonymous gets 401, an org owner
// 403, an admin reads the root.
func TestClusterTrustAPI(t *testing.T) {
	w := newWorld(t)
	for _, p := range []struct{ method, path string }{
		{"GET", "/admin/cluster-trust"},
		{"POST", "/admin/cluster-trust/rotate-root"},
		{"POST", "/admin/cluster-trust/finish-rollover"},
		{"POST", "/admin/cluster-trust/break-glass"},
		{"POST", "/admin/cluster-trust/revoke-controller-cert"},
	} {
		if code, _ := w.do(t, "", p.method, p.path, ""); code != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, code)
		}
		if code, _ := w.do(t, w.owner, p.method, p.path, ""); code != 403 {
			t.Errorf("%s %s owner = %d, want 403", p.method, p.path, code)
		}
	}
	code, body := w.do(t, w.admin, "GET", "/admin/cluster-trust", "")
	for _, want := range []string{`"fingerprint":"`, `"rolling_over":false`, `"on_old_root":[]`, `"not_rejoined":[]`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("admin trust = %d, no %s in %s", code, want, body)
		}
	}
}
