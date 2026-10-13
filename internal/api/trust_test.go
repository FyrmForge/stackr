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

// An admin runs every trust verb end to end: rotate (a second refused while
// one runs; a worker on the old root keeps it running), the forced finish,
// revoke the controller cert, break glass (a second within the gap refused).
func TestClusterTrustAPIVerbs(t *testing.T) {
	w := newWorld(t)
	fp := func() string {
		_, body := w.do(t, w.admin, "GET", "/admin/cluster-trust", "")
		i := strings.Index(body, `"fingerprint":"`) + len(`"fingerprint":"`)
		return body[i : i+64]
	}
	before := fp()
	w.env.Worker(t, "w1", before)
	steps := []struct {
		path string
		code int
		want string // in the trust read after
	}{
		{"rotate-root", 204, `"on_old_root":[{`},
		{"rotate-root", 409, `"rolling_over":true`},
		{"finish-rollover", 204, `"rolling_over":false`},
		{"finish-rollover", 409, `"rolling_over":false`},
		{"revoke-controller-cert", 204, `"rolling_over":false`},
		{"break-glass", 204, `"not_rejoined":[{`},
		{"break-glass", 409, `"break_glass_at":"`},
	}
	for _, s := range steps {
		if code, body := w.do(t, w.admin, "POST", "/admin/cluster-trust/"+s.path, ""); code != s.code {
			t.Fatalf("POST %s = %d %s, want %d", s.path, code, body, s.code)
		}
		if _, body := w.do(t, w.admin, "GET", "/admin/cluster-trust", ""); !strings.Contains(body, s.want) {
			t.Fatalf("after %s: no %s in %s", s.path, s.want, body)
		}
		if s.path == "rotate-root" && fp() == before {
			t.Fatal("rotate-root kept the root")
		}
	}
}
