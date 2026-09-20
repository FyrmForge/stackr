// Package invite handles org invite links: an existing session joins the org
// directly, a fresh visitor creates an account through the invite.
package invite

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	hamrauth "github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/auth"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store          repo.Store
	authService    *service.AuthService
	sessionManager *hamrauth.SessionManager
	orgs           *service.OrgService
	members        *service.MemberService
}

func NewHandler(store repo.Store, authService *service.AuthService, sm *hamrauth.SessionManager) *handler {
	return &handler{store: store, authService: authService, sessionManager: sm}
}

// loadInvite fetches a live (unused, unexpired) invite and its org.
func (h *handler) loadInvite(ctx context.Context, token string) (*repo.Invite, *repo.Org) {
	inv, err := h.store.GetInvite(ctx, token)
	if err != nil || inv == nil || inv.UsedAt.Valid || time.Now().After(inv.ExpiresAt) {
		return nil, nil
	}
	org, err := h.orgs.Get(ctx, inv.OrgID)
	if err != nil {
		return nil, nil
	}
	return inv, org
}

// GET /invite/:token
func (h *handler) Page(c echo.Context) error {
	ctx := c.Request().Context()
	inv, org := h.loadInvite(ctx, c.Param("token"))
	if inv == nil {
		return respond.HTML(c, http.StatusNotFound, invalidPage(c))
	}
	if u := stackrmw.CurrentUser(c); u != nil {
		return h.join(c, inv, org, u)
	}
	f := JoinForm{Email: inv.Email}
	return respond.HTML(c, http.StatusOK, invitePage(c, org, inv, f, nil))
}

// POST /invite/:token, create the account and join.
func (h *handler) Submit(c echo.Context) error {
	ctx := c.Request().Context()
	inv, org := h.loadInvite(ctx, c.Param("token"))
	if inv == nil {
		return respond.HTML(c, http.StatusNotFound, invalidPage(c))
	}
	if u := stackrmw.CurrentUser(c); u != nil {
		return h.join(c, inv, org, u)
	}
	var f JoinForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
	}
	f.Email = strings.ToLower(strings.TrimSpace(f.Email))
	errs := map[string]string{}
	// A restricted invite is for one address. Silently swapping in the invited
	// address would hand someone an account under an email they never typed, so
	// a mismatch is refused instead.
	if inv.Email != "" && f.Email != "" && !strings.EqualFold(inv.Email, f.Email) {
		errs["email"] = "This invitation is for " + inv.Email
	}
	if inv.Email != "" && f.Email == "" {
		f.Email = strings.ToLower(inv.Email)
	}
	if f.Name == "" {
		errs["name"] = "Name is required"
	}
	if f.Email == "" {
		errs["email"] = "Email is required"
	}
	if len(f.Password) < 8 {
		errs["password"] = "At least 8 characters"
	}
	if f.Password != c.FormValue("confirm_password") {
		errs["confirm_password"] = "Passwords do not match"
	}
	if len(errs) > 0 {
		return respond.HTML(c, http.StatusUnprocessableEntity, invitePage(c, org, inv, f, errs))
	}
	user, err := h.authService.Register(ctx, f.Email, f.Password, f.Name)
	if err != nil {
		if errors.Is(err, service.ErrEmailTaken) {
			errs["email"] = "An account with this email already exists. Log in first, then open the invite link again"
		} else {
			errs["general"] = "Registration failed. Please try again."
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, invitePage(c, org, inv, f, errs))
	}
	session, err := h.sessionManager.CreateSession(ctx, user.ID, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "session error")
	}
	auth.SetSession(c, h.sessionManager, session)
	return h.join(c, inv, org, user)
}

// join adds the membership, consumes the invite, and lands in the new org.
func (h *handler) join(c echo.Context, inv *repo.Invite, org *repo.Org, u *repo.User) error {
	ctx := c.Request().Context()
	if inv.Email != "" && !strings.EqualFold(inv.Email, u.Email) {
		middleware.SetFlash(c, "This invite is for "+inv.Email+".", middleware.FlashError)
		return respond.Redirect(c, "/")
	}
	if role, _ := h.members.RoleOf(ctx, inv.OrgID, u.ID); role == "" {
		if err := h.store.UpsertOrgMember(ctx, &repo.OrgMember{
			OrgID: inv.OrgID, UserID: u.ID, Role: inv.Role, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	_ = h.store.MarkInviteUsed(ctx, inv.ID)
	c.SetCookie(&http.Cookie{
		Name: stackrmw.OrgCookie, Value: inv.OrgID, Path: "/",
		MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: stackrmw.SecureCookie(c),
		SameSite: http.SameSiteLaxMode,
	})
	middleware.SetFlash(c, "Welcome to "+org.Name+"!", middleware.FlashSuccess)
	return respond.Redirect(c, "/")
}

// WithOrgs gives the invite page the organization service.
func (h *handler) WithOrgs(o *service.OrgService) *handler { h.orgs = o; return h }

// WithMembers gives the invite page the membership service.
func (h *handler) WithMembers(m *service.MemberService) *handler { h.members = m; return h }
