// Package invite is the invite link's page: accept it signed in, or
// register through it.
package invite

import (
	"errors"
	"net/http"

	hamrauth "github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/auth"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler {
	return &handler{orch: orch}
}

func base(c echo.Context) string {
	return "/invite/" + c.Param("token")
}

// GET /invite/:token
func (h *handler) Page(c echo.Context) error {
	inv, err := h.orch.LookupInvite(c.Request().Context(), c.Param("token"))
	if errors.Is(err, errs.ErrNotFound) {
		return render.Page(c, http.StatusNotFound, "Invite", page(View{Gone: true}))
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := View{
		Base:   base(c),
		Role:   inv.Role,
		Email:  inv.Email,
		Signed: middleware.Principal(c) != nil,
		Form:   Form{Email: inv.Email},
	}
	return render.Page(c, http.StatusOK, "Invite", page(v))
}

// POST /invite/:token: the signed-in user joins.
func (h *handler) Accept(c echo.Context) error {
	ctx, token := c.Request().Context(), c.Param("token")
	inv, err := h.orch.LookupInvite(ctx, token)
	if err == nil {
		err = h.orch.AcceptInvite(ctx, token, middleware.Principal(c).User.ID)
	}
	if err != nil {
		msg, ok := render.Refused(err)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			msg = "This invite link is used or expired."
		case !ok:
			return middleware.HTTPError(err)
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, accept(base(c), msg))
	}
	return h.toOrg(c, middleware.Principal(c).User.ID, inv.OrgID)
}

// POST /invite/:token/register: the account and the join are one verb.
func (h *handler) Register(c echo.Context) error {
	ctx, token := c.Request().Context(), c.Param("token")
	f := Form{Name: c.FormValue("name"), Email: c.FormValue("email")}
	inv, err := h.orch.LookupInvite(ctx, token)
	var s *hamrauth.Session
	if err == nil {
		s, err = h.orch.RegisterInvited(ctx, token, f.Email, c.FormValue("password"), f.Name)
	}
	if err != nil {
		msg, refused := render.Refused(err)
		bad, invalid := errs.IsInvalid(err)
		switch {
		case invalid && bad.Field != "":
			f.Errors = map[string]string{bad.Field: bad.Msg}
		case refused:
			f.Errors = map[string]string{"general": msg}
		case errors.Is(err, errs.ErrNotFound):
			f.Errors = map[string]string{"general": "This invite link is used or expired."}
		default:
			return middleware.HTTPError(err)
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, register(base(c), f))
	}
	auth.SetSession(c, h.orch.Sessions(), s)
	return h.toOrg(c, s.SubjectID, inv.OrgID)
}

// toOrg sends the new member to the org's canvas.
func (h *handler) toOrg(c echo.Context, userID, orgID string) error {
	orgs, err := h.orch.Orgs(c.Request().Context(), userID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	for _, o := range orgs {
		if o.ID == orgID {
			return respond.Redirect(c, "/"+o.Slug)
		}
	}
	return respond.Redirect(c, "/")
}
