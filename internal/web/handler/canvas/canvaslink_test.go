package canvas

import (
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// The env drawer's "open the canvas" link is gone while the page already is the canvas.
func TestCanvasLinkHiddenOnTheCanvas(t *testing.T) {
	for _, tc := range []struct{ path, current, want string }{
		{"/acme/shop/dev/-/drawer", "https://p.test/acme/shop/dev?drawer=env:e1", ""},
		{"/acme/shop/dev/-/drawer", "https://p.test/acme/shop?drawer=env:e1", "/acme/shop/dev"},
		{"/acme/shop/dev", "", ""},
		{"/acme/shop/dev/-/drawer", "", "/acme/shop/dev"},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.current != "" {
			req.Header.Set("HX-Current-URL", tc.current)
		}
		if got := canvasLink(echo.New().NewContext(req, httptest.NewRecorder()), "/acme/shop/dev"); got != tc.want {
			t.Errorf("%s from %q: %q, want %q", tc.path, tc.current, got, tc.want)
		}
	}
}
