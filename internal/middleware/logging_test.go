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

// A client's request id is used only when it is short and plain.
func TestLoggingBoundsTheRequestID(t *testing.T) {
	e := echo.New()
	e.GET("/", func(c echo.Context) error { return c.NoContent(http.StatusOK) }, Logging())
	for id, keep := range map[string]bool{"abc-123_x.y": true, strings.Repeat("a", 65): false, "a b": false} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(HeaderRequestID, id)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if got := rec.Header().Get(HeaderRequestID); (got == id) != keep {
			t.Errorf("id %q came back as %q, keep %v", id, got, keep)
		}
	}
}
