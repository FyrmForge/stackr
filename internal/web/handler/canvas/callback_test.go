package canvas_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// GitHub's callback: a visitor logs in first; a state that matches no
// pending connector is refused, never completed.
// ponytail: the happy path needs GitHub's manifest API; the VM smoke has it.
func TestGitHubCallback(t *testing.T) {
	s := webtest.New(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/settings/github/callback?code=c&state=s.n", nil))
	if rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/login?next=%2Fsettings%2Fgithub%2Fcallback%3Fcode%3Dc%26state%3Ds.n" {
		t.Errorf("visitor = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := s.Do(t, "GET", "/settings/github/callback?code=c&state=nope.n", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown connector = %d", rec.Code)
	}
}
