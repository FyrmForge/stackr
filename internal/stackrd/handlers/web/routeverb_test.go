package web_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/FyrmForge/hamr/pkg/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/api"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The route-verb test — point 18's safety net, landed on its own.
//
// Point 18 moves authorization onto the route: `requireVerb(service.VerbX)`
// beside the declaration rather than a gate helper inside the handler body,
// because the body is where forgetting happens. This test is what makes that
// safe to do: it walks every route Echo has and fails any mutating one that
// names no verb. A gate dropped during the migration stops compiling into a
// silently open route and starts failing here instead.
//
// Until point 18 runs, every route fails it. That failure list is the
// inventory of what the ~35 gate helpers enforce today, and it is parked in
// knownUngated below rather than in a t.Skip, so the net is already up: a NEW
// mutating route that names no verb fails immediately, and a route that grows
// one has to be taken off the list.
//
// How a route names its verb: `e.POST(...).Name = string(service.VerbX)`.
// Echo already carries a Name per route, defaulted to the handler's function
// name, so nothing new has to be built to hold the declaration and point 18's
// middleware can read the same field.

// mutating is the set this test has an opinion about. A GET that changes
// something is a bug of a different kind and not this test's business.
var mutating = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// unauthenticated routes carry their own credential and answer before there
// is a principal at all, so there is no org standing to check: a password, a
// signup, an invite token, a share-link token, a webhook signature, a node
// join key, or the CLI's pre-auth exchange.
func unauthenticated(path string) bool {
	switch path {
	case "/login", "/logout", "/register", "/cli/exchange":
		return true
	}
	for _, p := range []string{"/login/validate/", "/register/validate/", "/invite/", "/s/", "/hooks/", "/join/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// personal routes are authenticated but not org-rooted: they write the
// signed-in user's own row. The verb table is org-rooted by design (see
// 05-assumptions.md), so there is no verb to name here — what gates these is
// "it is yours", which is the session subject and nothing else.
//
// This is a decision point 18 has to make out loud rather than a finished
// answer: either these stay outside the table, or the ladder grows a rung
// that means "self".
func personal(path string) bool {
	switch path {
	case "/orgs/switch", "/cli/authorize":
		return true
	// The org-home canvas. These read as org routes and are not: they write
	// the caller's OWN saved layout (repo.ScopeUser, handler/org/graph.go),
	// so there is no org standing to check and no verb to name. Found by the
	// level capture — they were the only mutating routes left whose body
	// reached no gate at all.
	case "/graph/annotations", "/graph/annotations/delete",
		"/graph/groups", "/graph/groups/delete",
		"/graph/positions", "/graph/positions/reset":
		return true
	}
	return path == "/account" || path == "/notifications" ||
		strings.HasPrefix(path, "/account/") || strings.HasPrefix(path, "/notifications/")
}

// routes builds both surfaces onto one Echo and returns every mutating route
// that owes a verb. The dependencies are the ones registration itself
// dereferences; the handlers are never called, so the rest stays nil.
func routes(t *testing.T) []echo.Route {
	t.Helper()
	store := testdb.New(t)
	srv, err := server.New(server.WithDevMode(true))
	require.NoError(t, err, "server")
	hub := websocket.NewHub()
	t.Cleanup(hub.Close)
	web.RegisterRoutes(srv, &web.Deps{
		Store:          store,
		DevMode:        true,
		SessionManager: auth.NewSessionManager(store),
		AuthService:    service.NewAuthService(store),
		Hub:            hub,
		Access:         service.NewAccessService(store),
	})
	api.RegisterRoutes(srv, &api.Deps{Store: store})

	allRoutes = nil
	for _, r := range srv.Echo().Routes() {
		allRoutes = append(allRoutes, *r)
	}

	var owed []echo.Route
	for _, r := range srv.Echo().Routes() {
		if !mutating[r.Method] || unauthenticated(r.Path) || personal(r.Path) {
			continue
		}
		owed = append(owed, *r)
	}
	sort.Slice(owed, func(i, j int) bool {
		if owed[i].Path != owed[j].Path {
			return owed[i].Path < owed[j].Path
		}
		return owed[i].Method < owed[j].Method
	})
	return owed
}

func TestEveryMutatingRouteNamesAVerb(t *testing.T) {
	known := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(knownUngated), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			known[line] = true
		}
	}

	var missing, stale, current []string
	for _, r := range routes(t) {
		key := r.Method + " " + r.Path
		current = append(current, key)
		if service.KnownVerb(service.Verb(r.Name)) {
			if known[key] {
				stale = append(stale, key)
			}
			continue
		}
		if !known[key] {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		t.Errorf("%d mutating route(s) name no service.Verb and are not on the known list.\n"+
			"Either gate the route — e.POST(...).Name = string(service.VerbX) — or, if it is\n"+
			"genuinely unauthenticated or personal, add it to unauthenticated()/personal():\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d route(s) now name a verb but are still on the known-ungated list; remove them:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
	if t.Failed() {
		t.Logf("the full current list, to paste into knownUngated:\n%s", strings.Join(current, "\n"))
	}
	fmt.Printf("route-verb: %d mutating routes owe a verb, %d still ungated\n", len(current), len(known))
}

// knownUngated is the inventory: every mutating route whose authorization
// still lives in a gate helper inside the handler body. It is also written
// into docs/plans/service-extraction/06-points-18-20.md, grouped by area.
//
// Point 18 empties this list. Nothing here is a route that should be open —
// it is a route whose level is decided somewhere a test cannot see.
const knownUngated = `

`

// allRoutes is every route on both surfaces, recorded while routes() builds
// them. routes() itself answers only the mutating ones that owe a verb; the
// read tests need the whole table, and building the router twice would double
// the fixture for nothing.
var allRoutes []echo.Route

func routesAll(t *testing.T) []echo.Route {
	t.Helper()
	if allRoutes == nil {
		routes(t)
	}
	return allRoutes
}
