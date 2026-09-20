package v1

// Members, roles and invites.
//
// Org administration was panel-only, so onboarding a person was a thing nobody
// could script. Adding a member is always an invite: a user row is created by
// accepting one, and letting an API mint memberships for addresses that have
// never signed in would hand out access to accounts that do not exist yet.

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// inviteDays is how long a link stays good. The same default the panel uses.
const inviteDays = 7

func (a *API) listMembers(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	ms, err := a.store.ListOrgMembers(c.Request().Context(), o.ID)
	if err != nil {
		return err
	}
	out := make([]memberOut, 0, len(ms))
	for i := range ms {
		out = append(out, memberOut{UserID: ms[i].UserID, Email: ms[i].Email,
			Name: ms[i].Name, Role: ms[i].Role})
	}
	return c.JSON(http.StatusOK, out)
}

// addMember invites rather than inserting a membership: a user row comes from
// accepting an invite, so minting one for an address that has never signed in
// would grant access to an account that does not exist.
func (a *API) addMember(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in inviteIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// The role whitelist, the expiry bounds and the already-a-member check
	// are the service's. A bad role used to become "member" here without
	// saying so, and a negative expiry minted an invite that had already run
	// out.
	inv, err := a.members.Invite(c.Request().Context(), o, in.Email, in.Role, in.ExpiresDays, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toInviteOut(inv))
}

func (a *API) setMemberRole(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in roleIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	ctx := c.Request().Context()
	if err := a.members.SetRole(ctx, o, c.Param("user"), in.Role); err != nil {
		return stackrmw.HTTP(err)
	}
	m, err := a.store.GetOrgMember(ctx, o.ID, c.Param("user"))
	if err != nil || m == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return c.JSON(http.StatusOK, memberOut{UserID: m.UserID, Email: m.Email, Name: m.Name, Role: m.Role})
}

func (a *API) removeMember(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.members.Remove(c.Request().Context(), o, c.Param("user")); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (a *API) listInvites(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	invs, err := a.store.ListInvitesByOrg(c.Request().Context(), o.ID)
	if err != nil {
		return err
	}
	out := make([]inviteOut, 0, len(invs))
	for i := range invs {
		out = append(out, toInviteOut(&invs[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) createInvite(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in inviteIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	inv, err := a.newInvite(c, o, strings.ToLower(strings.TrimSpace(in.Email)),
		validRole(in.Role), in.ExpiresDays)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toInviteOut(inv))
}

func (a *API) deleteInvite(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	inv, err := a.store.GetInvite(ctx, c.Param("invite"))
	if err != nil || inv == nil || inv.OrgID != o.ID {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := a.store.DeleteInvite(ctx, inv.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// newInvite mints the row. The id is the token in the link, so it is what the
// caller has to carry to the person it is for.
func (a *API) newInvite(c echo.Context, o *repo.Org, email, role string, days int) (*repo.Invite, error) {
	if days <= 0 {
		days = inviteDays
	}
	now := time.Now().UTC()
	inv := &repo.Invite{ID: secrets.RandomHex(24), OrgID: o.ID, Email: email, Role: role,
		CreatedBy: a.user(c).ID, CreatedAt: now, ExpiresAt: now.AddDate(0, 0, days)}
	if err := a.store.CreateInvite(c.Request().Context(), inv); err != nil {
		return nil, err
	}
	// The panel mailed the invite and this path did not, so an invite created
	// over the API or the CLI wrote a row and told nobody. A bounce is not
	// fatal: the link is in the response either way.
	inv.MailFailed = a.mail.SendInvite(c.Request().Context(), o, inv)
	return inv, nil
}

func toInviteOut(i *repo.Invite) inviteOut {
	out := inviteOut{ID: i.ID, Email: i.Email, Role: i.Role,
		ExpiresAt: i.ExpiresAt, URL: "/invites/" + i.ID}
	out.Used = i.UsedAt.Valid
	return out
}

// validRole falls back to member: the least access that can still do work, so
// a typo grants less than was meant rather than more.
func validRole(r string) string {
	switch r {
	case "owner", "member", "viewer":
		return r
	}
	return "member"
}
