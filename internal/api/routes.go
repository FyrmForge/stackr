package api

import (
	"net/http"

	"github.com/FyrmForge/stackr/internal/api/handler/v1"
	"github.com/FyrmForge/stackr/internal/authz"
)

// Gates that are not an authz verb.
const (
	Self   authz.Verb = "@self"   // any live principal: /me, the org list
	Public authz.Verb = "@public" // no principal: exchange, invites
)

// Route is one /api/v1 endpoint. Verb is the authz verb the middleware
// checks, or Self / Public; a route without one does not mount.
type Route struct {
	Method, Path, Op string
	Verb             authz.Verb
	E                v1.Endpoint
}

const (
	org   = "/orgs/:org"
	stack = org + "/stacks/:stack"
	env   = stack + "/envs/:env"
	tile  = env + "/tiles/:tile"
)

// Routes is the whole API, the table the OpenAPI dump and the CLI coverage
// test read.
func Routes(h *v1.H) []Route {
	return []Route{
		{http.MethodGet, "/orgs", "org.list", Self, h.Orgs()},
		{http.MethodGet, org, "org.get", "org.read", h.GetOrg()},
	}
}
