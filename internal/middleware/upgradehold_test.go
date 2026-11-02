package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// While held, changes are refused with the upgrade line; reads and the
// exempt paths go through. Not held, everything goes through.
func TestUpgradeHold(t *testing.T) {
	e := echo.New()
	for _, tc := range []struct {
		held         bool
		method, path string
		refused      bool
	}{
		{true, http.MethodPost, "/acme/-/positions", true},
		{true, http.MethodDelete, "/api/v1/orgs/acme", true},
		{true, http.MethodGet, "/acme", false},
		{true, http.MethodPost, "/login", false},
		{true, http.MethodPost, "/login/validate/email", false},
		{true, http.MethodPost, "/api/v1/join/status", false},
		{true, http.MethodPost, "/loginx", true},
		{false, http.MethodPost, "/acme/-/positions", false},
	} {
		c := e.NewContext(httptest.NewRequest(tc.method, tc.path, nil), httptest.NewRecorder())
		h := UpgradeHold(func() bool { return tc.held }, "/login", "/api/v1/join")(func(echo.Context) error { return nil })
		err := h(c)
		var he *echo.HTTPError
		refused := errors.As(err, &he) && he.Code == http.StatusServiceUnavailable && he.Message == UpgradeHeldMsg
		if refused != tc.refused || (!refused && err != nil) {
			t.Errorf("held %v %s %s = %v", tc.held, tc.method, tc.path, err)
		}
	}
}
