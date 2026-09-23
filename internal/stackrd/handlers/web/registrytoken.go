package web

// The registry token endpoint. A docker client pushing or pulling gets a 401
// from the registry with a realm pointing here, presents its org registry
// credential over basic auth, and receives a JWT scoped to that org's
// namespace and nothing else.
//
// Deliberately outside the site group: docker is not a browser, it carries no
// session and no CSRF token, and a redirect to /login would come back as an
// unreadable parse error on the client.
//
// Who the client is and what it may reach is RegistryService.Authenticate's —
// three identities, two constant-time comparisons and an org check, none of
// which is a handler's to hold. What is left here is the docker protocol: the
// realm header that makes a client retry with credentials at all, and the
// response shape it expects back.

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
)

// tokenResponse is the shape the docker client expects back.
type tokenResponse struct {
	Token       string    `json:"token"`
	AccessToken string    `json:"access_token"` // older clients read this name
	ExpiresIn   int       `json:"expires_in"`
	IssuedAt    time.Time `json:"issued_at"`
}

// registryToken handles GET /v2/token?service=&scope=.
func registryToken(regs *service.RegistryService, signer *registry.Signer) echo.HandlerFunc {
	return func(c echo.Context) error {
		if signer == nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "registry token signing is not configured")
		}
		user, secret, ok := c.Request().BasicAuth()
		if !ok {
			// The realm header is what makes a docker client retry with
			// credentials instead of giving up.
			c.Response().Header().Set("WWW-Authenticate", `Basic realm="stackr registry"`)
			return echo.NewHTTPError(http.StatusUnauthorized, "credentials required")
		}
		login, err := regs.Authenticate(c.Request().Context(), user, secret, c.QueryParams()["scope"])
		if err != nil {
			return err
		}
		if login == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid registry credentials")
		}
		tok, exp, err := signer.Sign(login.Subject, login.Access)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, tokenResponse{Token: tok, AccessToken: tok,
			ExpiresIn: int(time.Until(exp).Seconds()), IssuedAt: time.Now().UTC()})
	}
}
