package middleware_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// A node that is down is a 503 that does not name the node (it is org-wide
// info); the detail rides along for the admin-only API body.
func TestHTTPErrorNodeDown(t *testing.T) {
	down := &service.NodeDownError{Name: "h3", LastSeen: time.Now().Add(-14 * time.Minute)}
	err := middleware.HTTPError(fmt.Errorf("deploy: %w", down))
	var he *echo.HTTPError
	if !errors.As(err, &he) || he.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTPError = %v; want a 503", err)
	}
	if got := fmt.Sprint(he.Message); got != middleware.NodeOfflineMsg || strings.Contains(got, "h3") {
		t.Errorf("message = %q", got)
	}
	if !errors.Is(he.Internal, down) {
		t.Errorf("internal = %v, want the node-down error", he.Internal)
	}
}
