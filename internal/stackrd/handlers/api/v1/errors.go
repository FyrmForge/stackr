package v1

import (
	"context"
	"net/http"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/labstack/echo/v4"
)

// managedGuard rejects a structural write when the config file owns the stack,
// a config plan would revert it (the silent-drift bug). Provisioning, secrets
// and certs are not structural and don't use this.
func managedGuard(s *repo.Stack) error {
	if s != nil && s.ConfigManaged() {
		return echo.NewHTTPError(http.StatusConflict, "stack is managed by its config file; edit the file to change its structure")
	}
	return nil
}

// rejectManaged is managedGuard for handlers that hold a tile, not the stack.
// Fails closed: if the stack can't be read we can't prove the write is allowed.
func (a *API) rejectManaged(ctx context.Context, stackID string) error {
	s, err := a.stacks.Get(ctx, stackID)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return managedGuard(s)
}

// errBody is the uniform API error shape: {"error": {"code": N, "message": ...}}.
type errBody struct {
	Error errDetail `json:"error"`
}

type errDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSONErrors is the outermost v1 middleware: it renders any handler error as
// the uniform JSON shape instead of echo's default HTML/text, so every failure
// looks the same to a CLI. Internal errors are collapsed to a generic message
// (status + text) so raw error strings don't leak.
func (a *API) JSONErrors(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := next(c)
		if err == nil || c.Response().Committed {
			return err
		}
		code := http.StatusInternalServerError
		msg := http.StatusText(code)
		if he, ok := err.(*echo.HTTPError); ok {
			code = he.Code
			if m, ok := he.Message.(string); ok {
				msg = m
			} else {
				msg = http.StatusText(code)
			}
		}
		return c.JSON(code, errBody{Error: errDetail{Code: code, Message: msg}})
	}
}
