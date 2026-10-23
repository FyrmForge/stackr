package connpanel

import (
	"errors"
	"fmt"
	"net/http"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// Callback is GET service.ConnectorCallbackPath: the one route a provider
// sends the browser back to from a redirect setup. The state, and the
// signed-in user matching who began, prove the caller began it. Done, the
// browser goes back where the setup began; a refusal is a flash on the
// home canvas, a real failure the error page.
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
		return c.Redirect(http.StatusSeeOther, ret)
	}
}
