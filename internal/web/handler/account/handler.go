// Package account is the signed-in user's own page: change the password,
// see and revoke their API keys.
package account

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

func me(c echo.Context) service.User { return middleware.Principal(c).User }

// GET /account
func (h *handler) Page(c echo.Context) error {
	keys, err := h.keys(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return render.Page(c, http.StatusOK, "Account", page(me(c).Email, PasswordView{}, keys))
}

// POST /account/password (current_password, password)
func (h *handler) Password(c echo.Context) error {
	err := h.orch.ChangePassword(c.Request().Context(), me(c).ID, c.FormValue("current_password"), c.FormValue("password"))
	if err == nil {
		return respond.HTML(c, http.StatusOK, password(PasswordView{Done: true}))
	}
	msg, ok := render.Refused(err)
	if !ok {
		return middleware.HTTPError(err)
	}
	v := PasswordView{Errors: map[string]string{"general": msg}}
	if bad, ok := errs.IsInvalid(err); ok && bad.Field != "" {
		v.Errors = map[string]string{bad.Field: bad.Msg}
	}
	return respond.HTML(c, http.StatusUnprocessableEntity, password(v))
}

// POST /account/keys/:key/revoke answers the key list.
func (h *handler) Revoke(c echo.Context) error {
	err := h.orch.RevokeKey(c.Request().Context(), me(c).ID, c.Param("key"))
	msg, ok := render.Refused(err)
	if err != nil && !ok {
		return middleware.HTTPError(err)
	}
	v, lerr := h.keys(c)
	if lerr != nil {
		return middleware.HTTPError(lerr)
	}
	v.Error = msg
	return respond.HTML(c, http.StatusOK, keyList(v))
}

func (h *handler) keys(c echo.Context) (KeysView, error) {
	ctx, u := c.Request().Context(), me(c)
	ks, err := h.orch.Keys(ctx, u.ID)
	if err != nil {
		return KeysView{}, err
	}
	orgs, err := h.orch.Orgs(ctx, u.ID)
	if err != nil {
		return KeysView{}, err
	}
	names := map[string]string{}
	for _, o := range orgs {
		names[o.ID] = o.Name
	}
	var v KeysView
	for _, k := range ks {
		org := "any (admin)"
		if k.OrgID != nil {
			org = names[*k.OrgID]
		}
		v.Rows = append(v.Rows, Key{ID: k.ID, Name: k.Name, Org: org, Created: k.CreatedAt.Local().Format("Jan 2 2006")})
	}
	return v, nil
}
