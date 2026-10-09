package stream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// A plain GET and a cross-origin upgrade are refused before the caller runs
// anything; a same-origin upgrade passes.
func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		upgrade      bool
		want         int
		mod          func(*http.Request)
	}{
		{"plain get", "", false, http.StatusUpgradeRequired, nil},
		{"cross origin", "https://evil.example", true, http.StatusForbidden, nil},
		{"no origin", "", true, 0, nil},
		{"same origin", "https://panel.example", true, 0, nil},
		{"token list", "", true, 0, func(r *http.Request) { r.Header.Set("Connection", "keep-alive, Upgrade") }},
		{"no key", "", true, http.StatusBadRequest, func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }},
		{"short key", "", true, http.StatusBadRequest, func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "abc") }},
		{"wrong version", "", true, http.StatusBadRequest, func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }},
		{"post", "", true, http.StatusMethodNotAllowed, func(r *http.Request) { r.Method = http.MethodPost }},
		{"notupgrade", "", true, http.StatusUpgradeRequired, func(r *http.Request) { r.Header.Set("Connection", "notupgrade") }},
		{"upgrade not websocket", "", true, http.StatusUpgradeRequired, func(r *http.Request) { r.Header.Set("Upgrade", "h2c") }},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://panel.example/terminal?shell=reboot", nil)
		if tc.upgrade {
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Sec-WebSocket-Version", "13")
			r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		}
		if tc.mod != nil {
			tc.mod(r)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		err := Check(echo.New().NewContext(r, httptest.NewRecorder()))
		got := 0
		if he, ok := err.(*echo.HTTPError); ok {
			got = he.Code
		}
		if got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}
