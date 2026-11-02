package middleware

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// UpgradeHeldMsg is what a change hears while an upgrade helper runs.
const UpgradeHeldMsg = "An upgrade is underway. Try again in a minute."

// UpgradeHold refuses changes (any method but GET, HEAD and OPTIONS) while
// held says an upgrade helper runs: the new panel is on trial, and a
// rollback brings back the database from before the swap, so a change made
// now could be lost. Reads still work. A path in exempt, or under it, goes
// through: logging in and out, and the nodes' own calls.
func UpgradeHold(held func() bool, exempt ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			switch c.Request().Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				return next(c)
			}
			if !held() {
				return next(c)
			}
			p := c.Request().URL.Path
			for _, e := range exempt {
				if p == e || strings.HasPrefix(p, e+"/") {
					return next(c)
				}
			}
			return echo.NewHTTPError(http.StatusServiceUnavailable, UpgradeHeldMsg)
		}
	}
}
