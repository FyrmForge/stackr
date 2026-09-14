package web

import (
	"net/http"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
)

// htmxErrors keeps a rejected htmx form on screen. htmx swaps 4xx bodies (see
// htmx.config.responseHandling in layout.templ), so a validation error would
// otherwise blow the whole error page into the form's target, taking the
// user's input and the CSRF field with it. For htmx requests we answer a 4xx
// with "swap nothing" plus an out-of-band toast; everything else (navigation,
// 5xx) falls through to the error page middleware.
//
// Registered inside hamrmw.ErrorPages so this sees the error first.
func htmxErrors() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			if err == nil || c.Response().Committed {
				return err
			}
			if c.Request().Header.Get("HX-Request") != "true" {
				return err
			}
			he, ok := err.(*echo.HTTPError)
			if !ok || he.Code < 400 || he.Code >= 500 {
				return err
			}
			msg, ok := he.Message.(string)
			if !ok {
				msg = http.StatusText(he.Code)
			}
			c.Response().Header().Set("HX-Reswap", "none")
			return respond.HTML(c, he.Code, components.ToastOOB(msg, hamrmw.FlashError))
		}
	}
}
