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
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// inviteDays is how long a link stays good. The same default the panel uses.
const inviteDays = 7

// requireOrgOwner is the gate on membership changes: content write is not
// enough to hand somebody else access.
func (a *API) requireOrgOwner(c echo.Context) (*repo.Org, error) {
	o, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return nil, err
	}
	if a.isAdmin(c) {
		return o, nil
	}
	m, err := a.store.GetOrgMember(c.Request().Context(), o.ID, a.user(c).ID)
	if err != nil || m == nil || m.Role != "owner" {
		return nil, echo.NewHTTPError(http.StatusForbidden, "only an owner can change membership")
	}
	return o, nil
}

func (a *API) listMembers(c echo.Context) error {
	o, err := a.requireOrg(c, c.Param("id"))
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
	o, err := a.requireOrgOwner(c)
	if err != nil {
		return err
	}
	var in inviteIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "email required")
	}
	role := validRole(in.Role)
	// An existing member's role is changed, not re-invited: a second invite to
	// somebody who is already here reads as an error the caller cannot see.
	ms, err := a.store.ListOrgMembers(c.Request().Context(), o.ID)
	if err != nil {
		return err
	}
	for i := range ms {
		if strings.EqualFold(ms[i].Email, email) {
			return echo.NewHTTPError(http.StatusConflict,
				email+" is already a member; change their role instead")
		}
	}
	inv, err := a.newInvite(c, o, email, role, in.ExpiresDays)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toInviteOut(inv))
}

func (a *API) setMemberRole(c echo.Context) error {
	o, err := a.requireOrgOwner(c)
	if err != nil {
		return err
	}
	var in roleIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	ctx := c.Request().Context()
	m, err := a.store.GetOrgMember(ctx, o.ID, c.Param("user"))
	if err != nil || m == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	role := validRole(in.Role)
	if m.Role == "owner" && role != "owner" {
		if err := a.lastOwnerGuard(c, o, m.UserID); err != nil {
			return err
		}
	}
	m.Role = role
	if err := a.store.UpsertOrgMember(ctx, m); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, memberOut{UserID: m.UserID, Email: m.Email, Name: m.Name, Role: m.Role})
}

func (a *API) removeMember(c echo.Context) error {
	o, err := a.requireOrgOwner(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	m, err := a.store.GetOrgMember(ctx, o.ID, c.Param("user"))
	if err != nil || m == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if m.Role == "owner" {
		if err := a.lastOwnerGuard(c, o, m.UserID); err != nil {
			return err
		}
	}
	if err := a.store.DeleteOrgMember(ctx, o.ID, m.UserID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// lastOwnerGuard refuses to leave an org with nobody who can administer it.
// An admin can still reach it, but from inside the product the org would be
// stuck: nobody left to invite anybody.
func (a *API) lastOwnerGuard(c echo.Context, o *repo.Org, userID string) error {
	ms, err := a.store.ListOrgMembers(c.Request().Context(), o.ID)
	if err != nil {
		return err
	}
	for i := range ms {
		if ms[i].Role == "owner" && ms[i].UserID != userID {
			return nil
		}
	}
	return echo.NewHTTPError(http.StatusConflict,
		"this is the last owner; promote somebody else first")
}

func (a *API) listInvites(c echo.Context) error {
	o, err := a.requireOrgOwner(c)
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
	o, err := a.requireOrgOwner(c)
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
	o, err := a.requireOrgOwner(c)
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
