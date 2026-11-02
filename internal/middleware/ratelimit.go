package middleware

import (
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/labstack/echo/v4"
)

// Per-caller budget for /api/v1: a minute's worth plus a burst. Generous
// enough for a CI loop polling a job, small enough that one runaway key
// cannot starve the panel.
const (
	apiRate  = 600
	apiBurst = 100
)

// APIRateLimit is a token budget per API key (a browser session counts as
// its user, an anonymous call as its address). Mount after Load. Over the
// budget is a 429 with Retry-After.
func APIRateLimit() echo.MiddlewareFunc {
	return hamrmw.RateLimitWithConfig(hamrmw.RateLimitConfig{
		Store: hamrmw.NewMemoryStore(hamrmw.WithMaxSize(10_000)),
		Rate:  apiRate,
		Burst: apiBurst,
		KeyFunc: func(c echo.Context) (string, error) {
			switch p := Principal(c); {
			case p == nil:
				return "ip:" + c.RealIP(), nil
			case p.KeyID != "":
				return "key:" + p.KeyID, nil
			default:
				return "user:" + p.User.ID, nil
			}
		},
	})
}
