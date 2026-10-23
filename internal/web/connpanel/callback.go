package connpanel

import (
	"errors"
	"fmt"
	"net/http"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// Callback is GET service.ConnectorCallbackPath (and the callback paths a
// kind's apps were registered with before, service.ConnectorLegacyBack):
// the one route a provider sends the browser back to from a redirect
// setup. The state, and the signed-in user matching who began, prove the
// caller began it. Done, the browser goes on: to the provider's install
// page for a kind installed there, else back where the setup began; a
// refusal is a flash on the home canvas, a real failure the error page.
func Callback(orch *service.Orchestrator) echo.HandlerFunc {
	return func(c echo.Context) error {
		ret, err := orch.ConnectorBack(c.Request().Context(), middleware.Principal(c).User.ID, c.QueryParams())
		if err != nil {
			var he *echo.HTTPError
			if !errors.As(middleware.HTTPError(err), &he) {
				return err
			}
			hamrmw.SetFlash(c, "Connection failed: "+fmt.Sprint(he.Message), hamrmw.FlashError)
			return c.Redirect(http.StatusSeeOther, "/")
		}
		hamrmw.SetFlash(c, "Connected.", hamrmw.FlashSuccess)
		if ret == "" {
			ret = "/"
		}
		return c.Redirect(http.StatusSeeOther, ret)
	}
}

// Installed is GET service.ConnectorReturnPath: a provider sends the
// browser back to a connection after a step on its own site (an install,
// or a later change of its repos). The state it carried names the panel
// path the step began on; with none (a change made at the provider), back
// is where to go: the level the connection lives on. Each of those pages
// checks access itself.
func Installed(orch *service.Orchestrator, back func(c echo.Context, orgID *string, id string) (string, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx, id := c.Request().Context(), c.Param("connector")
		orgID, err := orch.ConnectorOrgID(ctx, id)
		if errors.Is(err, errs.ErrNotFound) {
			return c.Redirect(http.StatusSeeOther, "/")
		}
		if err != nil {
			return middleware.HTTPError(err)
		}
		label := c.Param("kind")
		if k, err := orch.ConnectorKindOf(label); err == nil {
			label = k.Label
		}
		hamrmw.SetFlash(c, label+" installed.", hamrmw.FlashSuccess)
		if to, ok := orch.ReturnTo(c.QueryParam("state")); ok {
			return c.Redirect(http.StatusSeeOther, to)
		}
		to, err := back(c, orgID, id)
		if err != nil {
			return middleware.HTTPError(err)
		}
		return c.Redirect(http.StatusSeeOther, to)
	}
}
