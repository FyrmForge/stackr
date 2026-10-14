package middleware_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// A node that is down is a 503 that says which one and since when, not a bare 503.
func TestHTTPErrorNodeDown(t *testing.T) {
	down := &service.NodeDownError{Name: "h3", LastSeen: time.Now().Add(-14 * time.Minute)}
	err := middleware.HTTPError(fmt.Errorf("deploy: %w", down))
	var he *echo.HTTPError
	if !errors.As(err, &he) || he.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTPError = %v; want a 503", err)
	}
	if got, want := fmt.Sprint(he.Message), down.Error(); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}
