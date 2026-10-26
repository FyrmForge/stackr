package canvas

import (
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// A drawer asked for a tab the viewer cannot see shows its first tab, and
// the page URL follows; the tab asked for and shown leaves it alone.
func TestFixTabURL(t *testing.T) {
	for _, tc := range []struct{ asked, shown, want string }{
		{"connectors", "general", "/acme?drawer=org%3Ao1&tab=general"},
		{"general", "general", ""},
	} {
		req := httptest.NewRequest("GET", "/acme/-/drawer?tab="+tc.asked, nil)
		req.Header.Set("HX-Request", "true")
		req.Header.Set("HX-Current-URL", "https://panel.test/acme?drawer=org:o1&tab="+tc.asked+"&open=x")
		rec := httptest.NewRecorder()
		fixTabURL(echo.New().NewContext(req, rec), tc.shown)
		if got := rec.Header().Get("HX-Replace-Url"); got != tc.want {
			t.Errorf("asked %s: replace %q, want %q", tc.asked, got, tc.want)
		}
	}
}
