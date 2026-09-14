package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ManagedErr is the 409 returned when a structural write is blocked because the
// config file owns the stack, a config plan would revert it (the silent-drift
// bug). Secrets, certs and provisioning are not structural and don't use this.
func ManagedErr(s *repo.Stack) *echo.HTTPError {
	ref := s.ConfigRepo
	if ref == "" {
		ref = "the config file"
	}
	return echo.NewHTTPError(http.StatusConflict, "stack is managed by "+ref+"; edit the config file to change its structure")
}

// RequireUnmanaged blocks a structural write when the config file owns the
// stack. It fails closed: a lookup error blocks the write rather than letting
// it through, since "can't tell" must not become "allowed" on a locked stack.
func RequireUnmanaged(c echo.Context, store repo.Store, stackID string) error {
	s, err := store.GetStack(c.Request().Context(), stackID)
	if err != nil || s == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if s.ConfigManaged() {
		return ManagedErr(s)
	}
	return nil
}
