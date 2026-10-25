package canvas

import (
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// A rename keeps the viewer where they were: on the card's own page, that
// page at its new URL; anywhere else, the canvas the card sits on. Both
// open its drawer again and flash what happened.
func TestRenamedStaysOnPage(t *testing.T) {
	for _, tc := range []struct{ current, want string }{
		{"https://panel.test/acme/shop?drawer=stack:s1&tab=settings", "/acme/store?drawer=stack:s1&tab=settings&open=general"},
		{"https://panel.test/acme?drawer=stack:s1", "/acme?drawer=stack:s1&tab=settings&open=general"},
		{"", "/acme?drawer=stack:s1&tab=settings&open=general"},
	} {
		req := httptest.NewRequest("POST", "/acme/shop/-/drawer/rename", nil)
		req.Header.Set("HX-Current-URL", tc.current)
		rec := httptest.NewRecorder()
		if _, err := renamed(echo.New().NewContext(req, rec), "store", "/acme/shop", "/acme/store", "/acme", "drawer=stack:s1&tab=settings&open=general"); err != nil {
			t.Fatal(err)
		}
		if got := rec.Header().Get("HX-Redirect"); got != tc.want {
			t.Errorf("from %q: redirect %q, want %q", tc.current, got, tc.want)
		}
		if rec.Header().Get("Set-Cookie") == "" {
			t.Errorf("from %q: no flash", tc.current)
		}
	}
}
