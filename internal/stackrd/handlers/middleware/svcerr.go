package middleware

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
)

// HTTP translates the service error vocabulary into a response, once, for
// both surfaces (D4). Web and API both return echo errors, so one mapper
// covers both: the API's JSONErrors middleware renders an *echo.HTTPError as
// JSON, the web error page renders the same thing as HTML.
//
// Anything that is not a service error is passed through untouched — a store
// or docker failure is a 500 and must keep its own logging path.
func HTTP(err error) error {
	if err == nil {
		return nil
	}
	var inv svcerr.Invalid
	if errors.As(err, &inv) {
		return echo.NewHTTPError(http.StatusBadRequest, inv.Error())
	}
	var cf svcerr.Conflict
	if errors.As(err, &cf) {
		return echo.NewHTTPError(http.StatusConflict, cf.Msg)
	}
	var fb svcerr.Forbidden
	if errors.As(err, &fb) {
		return echo.NewHTTPError(http.StatusForbidden, fb.Msg)
	}
	switch {
	case errors.Is(err, svcerr.ErrNotFound):
		// Deliberately the same answer as "it exists but is not yours":
		// a 403 there would confirm the id to someone who should not have it.
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	case errors.Is(err, svcerr.ErrForbidden):
		return echo.NewHTTPError(http.StatusForbidden, "forbidden")
	case errors.Is(err, svcerr.ErrUnavailable):
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
	}
	return err
}

// FlashRefusal turns a service refusal into a flash message and reports
// whether it was one. It exists for the panel's htmx forms: they post over
// htmx, which renders an error body nowhere, so returning a bare 400 or 409
// is a save that appears to do nothing at all. Anything that is not a refusal
// — a store failure, a docker failure — comes back false and belongs on the
// error page.
func FlashRefusal(c echo.Context, err error) bool {
	if err == nil {
		return false
	}
	var inv svcerr.Invalid
	var cf svcerr.Conflict
	switch {
	case errors.As(err, &inv):
		hamrmw.SetFlash(c, inv.Error(), hamrmw.FlashError)
	case errors.As(err, &cf):
		hamrmw.SetFlash(c, cf.Msg, hamrmw.FlashError)
	default:
		return false
	}
	return true
}
