package web

// The registry token endpoint. A docker client pushing or pulling gets a 401
// from the registry with a realm pointing here, presents its org registry
// credential over basic auth, and receives a JWT scoped to that org's
// namespace and nothing else.
//
// Deliberately outside the site group: docker is not a browser, it carries no
// session and no CSRF token, and a redirect to /login would come back as an
// unreadable parse error on the client.

import (
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// tokenResponse is the shape the docker client expects back.
type tokenResponse struct {
	Token       string    `json:"token"`
	AccessToken string    `json:"access_token"` // older clients read this name
	ExpiresIn   int       `json:"expires_in"`
	IssuedAt    time.Time `json:"issued_at"`
}

// registryToken handles GET /v2/token?service=&scope=.
func registryToken(store repo.Store, signer *registry.Signer) echo.HandlerFunc {
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
		ctx := c.Request().Context()
		// The admin root credential first: the agent image and the garbage
		// collector live outside every org's namespace, so a token scoped to
		// one org cannot reach them. It is the managed registry row's own
		// user, which is where the admin credential already lived.
		if reg, err := store.GetManagedRegistry(ctx); err == nil && reg != nil &&
			reg.Username != "" && user == reg.Username && subtle.ConstantTimeCompare([]byte(secret), []byte(reg.Password)) == 1 {
			tok, exp, err := signer.Sign(user, registry.GrantAll(c.QueryParams()["scope"]))
			if err != nil {
				return err
			}
			return c.JSON(http.StatusOK, tokenResponse{Token: tok, AccessToken: tok,
				ExpiresIn: int(time.Until(exp).Seconds()), IssuedAt: time.Now().UTC()})
		}
		// The agent's pull-only identity. Same derivation as an org's system
		// credential, so there is no row to keep and rotating the registry
		// password rotates it, and it reaches the agent image and nothing
		// else.
		if reg, err := store.GetManagedRegistry(ctx); err == nil && reg != nil && reg.Password != "" &&
			user == registry.AgentUser &&
			subtle.ConstantTimeCompare([]byte(secret), []byte(registry.AgentSecret(reg.Password))) == 1 {
			tok, exp, err := signer.Sign(user, registry.GrantAgentPull(c.QueryParams()["scope"]))
			if err != nil {
				return err
			}
			return c.JSON(http.StatusOK, tokenResponse{Token: tok, AccessToken: tok,
				ExpiresIn: int(time.Until(exp).Seconds()), IssuedAt: time.Now().UTC()})
		}
		cred, err := store.GetOrgRegistryCredentialByHash(ctx, registry.HashSecret(secret))
		if err != nil {
			return err
		}
		// The username is the org slug, so a credential presented against the
		// wrong org is refused even though the secret is real. Without this
		// check the slug in the request would be decoration.
		org, err := store.GetOrgBySlug(ctx, user)
		if err != nil {
			return err
		}
		if cred == nil || org == nil || cred.OrgID != org.ID {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid registry credentials")
		}
		_ = store.TouchOrgRegistryCredential(ctx, cred.ID)

		// Narrowed, never echoed back: handing the client the scope it asked
		// for is exactly the cross-tenant hole this replaces.
		access := registry.GrantFor(org.Slug, c.QueryParams()["scope"])
		tok, exp, err := signer.Sign(org.Slug+"/"+cred.Name, access)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, tokenResponse{Token: tok, AccessToken: tok,
			ExpiresIn: int(time.Until(exp).Seconds()), IssuedAt: time.Now().UTC()})
	}
}
