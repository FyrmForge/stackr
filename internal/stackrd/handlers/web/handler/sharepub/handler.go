// Package sharepub is the public face of a secret link: the pages someone
// with no account sees at /s/:token. Nothing here needs a session, so every
// route runs outside RequireAuth, and every state change is a POST, so a
// link previewer or a browser prefetch cannot burn a link by looking at it.
package sharepub

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/respond"

	"github.com/FyrmForge/stackr/internal/stackrd/config/sharelink"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store repo.Store
	vars  *service.VariableService
	// links owns the secret_links table. This package used to reach past it
	// to the store, which is the half of D-8 on the burn side.
	links *service.RevokeService
}

// WithLinks gives the public pages the service that owns a share link.
func (h *handler) WithLinks(r *service.RevokeService) *handler { h.links = r; return h }

func NewHandler(store repo.Store) *handler {
	return &handler{store: store}
}

// WithVariables gives the drop box the variable service, for the half of a
// submit that is not the write itself.
func (h *handler) WithVariables(v *service.VariableService) *handler { h.vars = v; return h }

// fieldInput is how a drop-box value arrives. Prefixed so a field named
// "passphrase" can't collide with the gate's own input.
const fieldPrefix = "f_"

// GET /s/:token
func (h *handler) Page(c echo.Context) error {
	l, err := sharelink.Open(c.Request().Context(), h.links, c.Param("token"))
	if err != nil {
		return h.dead(c, err, false)
	}
	return respond.HTML(c, http.StatusOK, linkPage(c, l, c.Param("token"), nil, ""))
}

// POST /s/:token, the only route that changes anything.
func (h *handler) Submit(c echo.Context) error {
	ctx := c.Request().Context()
	token := c.Param("token")
	l, err := sharelink.Open(ctx, h.links, token)
	if err != nil {
		return h.dead(c, err, true)
	}

	// Values are re-sent with every attempt: there is no session to park them
	// in, so a wrong passphrase must not cost the client their typed input.
	values := map[string]string{}
	for _, f := range l.FieldList() {
		values[f.Name] = c.FormValue(fieldPrefix + f.Name)
	}

	if err := sharelink.Unlock(ctx, h.links, l, c.FormValue("passphrase")); err != nil {
		if errors.Is(err, sharelink.ErrPass) {
			return respond.HTML(c, http.StatusUnprocessableEntity,
				linkCard(c, l, token, values, "That passphrase is not right."))
		}
		return h.dead(c, err, true)
	}

	if l.Kind == repo.LinkShare {
		vars, err := sharelink.Reveal(ctx, h.store, h.links, l)
		if err != nil {
			return h.dead(c, err, true)
		}
		return respond.HTML(c, http.StatusOK, revealCard(c, l, vars))
	}

	if err := sharelink.Submit(ctx, h.store, h.links, l, values); err != nil {
		if errors.Is(err, sharelink.ErrDead) {
			return h.dead(c, err, true)
		}
		return respond.HTML(c, http.StatusUnprocessableEntity,
			linkCard(c, l, token, values, "Fill in every field before sending."))
	}
	// The burn and the writes are one transaction, so the storing could not be
	// handed over — but everything after it can be, and this path used to skip
	// all of it: a drop box filled in by a client released no deploy parked on
	// the name and left a config-managed stack's plan stale.
	if h.vars != nil {
		names := make([]string, 0, len(values))
		for _, f := range l.FieldList() {
			names = append(names, f.Name)
		}
		if err := h.vars.Applied(ctx, service.VarOwner{Kind: l.OwnerKind, ID: l.OwnerID}, names); err != nil {
			return err
		}
	}
	return respond.HTML(c, http.StatusOK, doneCard(c))
}

// dead renders one thing for every reason a link won't open, expired,
// burned, revoked, locked, never existed. A stranger learns nothing about
// which. card is true for the htmx swap, false for a whole page load.
func (h *handler) dead(c echo.Context, err error, card bool) error {
	if !errors.Is(err, sharelink.ErrDead) {
		return err
	}
	if card {
		return respond.HTML(c, http.StatusOK, deadCard(c))
	}
	return respond.HTML(c, http.StatusOK, deadPage(c))
}
