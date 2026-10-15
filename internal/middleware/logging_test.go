package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// A token in the path is a credential: the request line logs the route.
func TestLoggingHidesTokens(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	e := echo.New()
	e.GET("/join/:token", func(c echo.Context) error { return c.NoContent(http.StatusNotFound) }, Logging())
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/join/stkjoin_secret", nil))
	if s := buf.String(); strings.Contains(s, "stkjoin_secret") || !strings.Contains(s, "path=/join/:token") {
		t.Errorf("log = %s", s)
	}
}
