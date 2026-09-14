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
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store repo.Store
}

func NewHandler(store repo.Store) *handler {
	return &handler{store: store}
}

// fieldInput is how a drop-box value arrives. Prefixed so a field named
// "passphrase" can't collide with the gate's own input.
const fieldPrefix = "f_"

// GET /s/:token
func (h *handler) Page(c echo.Context) error {
	l, err := sharelink.Open(c.Request().Context(), h.store, c.Param("token"))
	if err != nil {
		return h.dead(c, err, false)
	}
	return respond.HTML(c, http.StatusOK, linkPage(c, l, c.Param("token"), nil, ""))
}

// POST /s/:token, the only route that changes anything.
func (h *handler) Submit(c echo.Context) error {
	ctx := c.Request().Context()
	token := c.Param("token")
	l, err := sharelink.Open(ctx, h.store, token)
	if err != nil {
		return h.dead(c, err, true)
	}

	// Values are re-sent with every attempt: there is no session to park them
	// in, so a wrong passphrase must not cost the client their typed input.
	values := map[string]string{}
	for _, f := range l.FieldList() {
		values[f.Name] = c.FormValue(fieldPrefix + f.Name)
	}

	if err := sharelink.Unlock(ctx, h.store, l, c.FormValue("passphrase")); err != nil {
		if errors.Is(err, sharelink.ErrPass) {
			return respond.HTML(c, http.StatusUnprocessableEntity,
				linkCard(c, l, token, values, "That passphrase is not right."))
		}
		return h.dead(c, err, true)
	}

	if l.Kind == repo.LinkShare {
		vars, err := sharelink.Reveal(ctx, h.store, l)
		if err != nil {
			return h.dead(c, err, true)
		}
		return respond.HTML(c, http.StatusOK, revealCard(c, l, vars))
	}

	if err := sharelink.Submit(ctx, h.store, l, values); err != nil {
		if errors.Is(err, sharelink.ErrDead) {
			return h.dead(c, err, true)
		}
		return respond.HTML(c, http.StatusUnprocessableEntity,
			linkCard(c, l, token, values, "Fill in every field before sending."))
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
