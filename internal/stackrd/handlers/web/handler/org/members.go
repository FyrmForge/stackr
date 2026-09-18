package org

import (
	"context"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	hamremail "github.com/FyrmForge/hamr/pkg/email"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/avatar"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// This file is the org's administration: members, invite links, rename and
// delete. These are sections of the org settings page (see settings.go); the
// actions here redirect back to the section they belong to.

// ownerOf reports owner rights on one specific org (admins always qualify).
func (h *handler) ownerOf(c echo.Context, orgID string) bool {
	if stackrmw.IsAdmin(c) {
		return true
	}
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return false
	}
	m, err := h.store.GetOrgMember(c.Request().Context(), orgID, u.ID)
	return err == nil && m != nil && m.Role == "owner"
}

// ownedOrg loads the org and refuses anyone who isn't its owner. Not-found
// rather than forbidden: a non-owner has no business learning it exists.
func (h *handler) ownedOrg(c echo.Context) (*repo.Org, error) {
	// ownedOrg is reached from POST routes whose param is the slug, so it
	// resolves the same way the settings pages do.
	o, err := h.settingsOrg(c)
	if err != nil {
		return nil, err
	}
	if o == nil || !h.ownerOf(c, o.ID) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	return o, nil
}

// POST /orgs/:id/rename, redirects rather than re-rendering the drawer: the
// slug moves with the name, so every URL on the page behind it is now stale.
//
// Also registered as the wizard's name step (/orgs/:slug/setup/name), which is
// the same change made for the first time. On that route a refusal is a flash
// on the step rather than a 400: mid-wizard there is no settings page behind
// the form to read the error on, and htmx would swap the bare status text into
// the page.
func (h *handler) Rename(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.ownedOrg(c)
	if err != nil {
		return err
	}
	// Both refusals below land here, on the step or on the tab that posted.
	back := "/orgs/" + o.Slug + "/settings/general"
	if inSetup(c) {
		back = "/orgs/" + o.Slug + "/setup/name"
	}
	// In the wizard a refusal comes back as the step itself, with what was
	// typed still in the box and the reason under it. A 400 would leave htmx
	// with nothing to swap, and a redirect would hand back an empty field.
	refuse := func(msg string) error {
		if !inSetup(c) {
			return echo.NewHTTPError(http.StatusBadRequest, msg)
		}
		return respond.HTML(c, http.StatusOK, setupNameForm(c, o, c.FormValue("name"), msg))
	}
	name := c.FormValue("name")
	if name == "" {
		return refuse("Give the organization a name.")
	}
	slug := repo.Slugify(name)
	if slug == "" {
		// Slugify keeps letters and digits, so a name made only of punctuation
		// has no URL in it and the org would be unreachable.
		return refuse("That name needs at least one letter or digit, since it becomes the URL.")
	}
	if existing, _ := h.store.GetOrgBySlug(ctx, slug); existing != nil && existing.ID != o.ID {
		return refuse("Another organization already uses that name.")
	}
	// Anti-squat in reverse: renaming to a slug that some other org's domain
	// already leads with would hand this org that org's generated hostnames.
	if all, derr := h.store.ListDomainResources(ctx); derr == nil {
		// Our own resources are not somebody else's: an org owns its stacks'
		// domains too, and treating them as foreign refused a rename over a
		// hostname the renamer already holds.
		ours := map[string]bool{o.ID: true}
		if stacks, serr := h.store.ListStacksByOrg(ctx, o.ID); serr == nil {
			for i := range stacks {
				ours[stacks[i].ID] = true
			}
		}
		for _, r := range all {
			label, _, ok := strings.Cut(strings.TrimPrefix(r.Host, "*."), ".")
			if ok && label == slug && !ours[r.OwnerID] {
				return refuse("Another organization's domain already leads with \"" + slug + "\", so this name is not available.")
			}
		}
	}
	// The registry namespace is the org slug, and a docker registry has no
	// rename: moving it would mean re-tagging every image (a blob mount plus a
	// manifest push per tag) or orphaning them. Refusing the rename is the
	// smaller thing to be right about.
	if slug != o.Slug {
		has, herr := registry.OrgHasImages(ctx, h.store, o.ID)
		if herr != nil {
			return herr
		}
		if has {
			return refuse("This organization has images in the registry, and they are stored under \"" + o.Slug + "\". Renaming would orphan them.")
		}
	}
	// back was built from the slug in the URL, which is still the live one until
	// UpdateOrg lands: an early return after this line must keep using it, or
	// the browser goes to a page that does not exist and loses both the flash
	// and the rename.
	o.Name, o.Slug = name, slug
	if fh, err := c.FormFile("logo"); err == nil && fh != nil && h.files != nil {
		p, err := avatar.Replace(ctx, h.files, "orgs", o.ID, o.AvatarPath, fh)
		if err != nil {
			middleware.SetFlash(c, err.Error(), middleware.FlashError)
			return respond.Redirect(c, back) // still the old slug: UpdateOrg has not run
		}
		o.AvatarPath = p
	}
	if c.FormValue("remove_logo") == "1" && o.AvatarPath != "" && h.files != nil {
		_ = h.files.Delete(ctx, o.AvatarPath)
		o.AvatarPath = ""
	}
	if err := h.store.UpdateOrg(ctx, o); err != nil {
		return err
	}
	// In the wizard this is the first name the org has had, so there is nothing
	// to warn about and the step moves on. backTo is no use here: it points at
	// the step that just posted.
	if inSetup(c) {
		return respond.Redirect(c, setupNextURL(o, "name"))
	}
	middleware.SetFlash(c, "Organization renamed. Note its URLs changed with the slug.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/general")
}

// POST /orgs/:id/delete, refused while the org still holds stacks. Doubles as
// the wizard's Discard, so both refusals have to land somewhere an owner
// mid-setup can actually see: settings are closed until Finish.
func (h *handler) Delete(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.ownedOrg(c)
	if err != nil {
		return err
	}
	unfinished := o.SetupDoneAt == nil
	back := "/orgs/" + o.Slug + "/settings/general"
	if unfinished {
		back = "/orgs/" + o.Slug + "/setup/done"
	}
	// Typing a generated slug to throw away a draft is friction with nothing
	// behind it, and the wizard's Discard has only hx-confirm to ask with. The
	// stacks check below still refuses a draft whose config apply built
	// something, and a finished org still has to be typed out.
	if !unfinished {
		if err := components.RequireConfirm(c, o.Slug); err != nil {
			return err
		}
	}
	stacks, err := h.store.ListStacksByOrg(ctx, o.ID)
	if err != nil {
		return err
	}
	if len(stacks) > 0 {
		msg := "Move or delete this org's stacks first."
		if unfinished {
			// The config branch's apply builds real stacks before Finish, and
			// there is no stacks page to send them to while setup is open.
			msg = "This organization's config file already built stacks. Finish setup, then delete it from settings."
		}
		middleware.SetFlash(c, msg, middleware.FlashError)
		return respond.Redirect(c, back)
	}
	orgs, err := h.store.ListOrgs(ctx)
	if err != nil {
		return err
	}
	// A draft is never the last organization worth keeping: on a fresh install
	// it is the only one there is, and refusing would leave the wizard with no
	// way out at all.
	if len(orgs) <= 1 && !unfinished {
		middleware.SetFlash(c, "The last organization cannot be deleted.", middleware.FlashError)
		return respond.Redirect(c, back)
	}
	if err := h.store.DeleteOrg(ctx, o.ID); err != nil {
		return err
	}
	// Its builder and build cache go with it, or they sit on the disk for ever.
	if h.rt != nil {
		if err := h.rt.RemoveBuilder(ctx, deploy.BuilderFor(o.Slug)); err != nil {
			slog.Warn("org builder not removed", "org", o.Slug, "error", err)
		}
	}
	middleware.SetFlash(c, "Organization deleted.", middleware.FlashSuccess)
	// The org cookie may still name the deleted org; OrgContext only honours it
	// when it matches one of the user's own orgs and otherwise falls back to the
	// first, so a stale value costs nothing and clearing it here would be dead
	// code (internal/middleware/orgctx.go).
	return respond.Redirect(c, "/") // the root canvas, which no longer draws it
}

// POST /orgs/:id/members, invite one person by email, whoever they are.
//
// Always an invite, never a straight join, and always the same answer. Two
// reasons: joining an account to an org it never agreed to is not the owner's
// call, and branching on whether the address has an account made this an
// enumeration oracle open to every org owner (addCandidates is admin-only for
// exactly this reason). An existing account that opens the link joins on the
// spot.
func (h *handler) AddMember(c echo.Context) error {
	o, err := h.ownedOrg(c)
	if err != nil {
		return err
	}
	back := backTo(c, o, "/orgs/"+o.Slug+"/settings/members")
	role := c.FormValue("role")
	if role != "owner" && role != "member" && role != "viewer" {
		role = "member"
	}
	email := strings.TrimSpace(strings.ToLower(c.FormValue("email")))
	if email == "" {
		middleware.SetFlash(c, "An email address is required.", middleware.FlashError)
		return respond.Redirect(c, back)
	}
	inv, err := h.newInvite(c, o, email, role, inviteExpiryDays(c))
	if err != nil {
		return err
	}
	middleware.SetFlash(c, h.inviteFlash(c, o, inv), middleware.FlashSuccess)
	return respond.Redirect(c, back)
}

// newInvite creates the invite row and, when this server can send mail, mails
// the link to the address it is bound to.
func (h *handler) newInvite(c echo.Context, o *repo.Org, addr, role string, days int) (*repo.Invite, error) {
	ctx := c.Request().Context()
	var createdBy string
	if u := stackrmw.CurrentUser(c); u != nil {
		createdBy = u.ID
	}
	inv := &repo.Invite{
		ID:        secrets.RandomHex(24),
		OrgID:     o.ID,
		Email:     addr,
		Role:      role,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().AddDate(0, 0, days),
	}
	if err := h.store.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	h.mailInvite(c, o, inv)
	return inv, nil
}

// mailInvite sends the invite link to the address it is bound to. A send
// failure is not fatal, the row exists and the link is on the page, so the
// invite is still usable by hand, it only sets MailFailed so the page can say
// so. Resend calls this with the same token rather than minting a new one.
func (h *handler) mailInvite(c echo.Context, o *repo.Org, inv *repo.Invite) {
	if inv.Email == "" || !h.mail.Enabled() {
		return
	}
	link := inviteURL(c, inv.ID)
	if err := h.mail.Send(c.Request().Context(), hamremail.Addr("", inv.Email),
		"You have been invited to "+o.Name+" on stackr",
		"You have been invited to join "+o.Name+" as "+inv.Role+".\n\nOpen this link to accept:\n"+link+"\n\nThe link works once and expires on "+inv.ExpiresAt.Format("Jan 2 2006")+".",
		`<p>You have been invited to join <strong>`+html.EscapeString(o.Name)+`</strong> as `+inv.Role+`.</p>`+
			`<p><a href="`+html.EscapeString(link)+`">Accept the invitation</a></p>`+
			`<p>The link works once and expires on `+inv.ExpiresAt.Format("Jan 2 2006")+`.</p>`,
	); err != nil {
		c.Logger().Warnf("invite mail to %s: %v", inv.Email, err)
		inv.MailFailed = true
	}
}

// inviteFlash says what actually happened to the invite, which depends on
// whether this server can send mail. It points at the Copy invite button in
// the People list rather than "below": the flash sits above the list, so the
// link is never literally below it.
func (h *handler) inviteFlash(c echo.Context, o *repo.Org, inv *repo.Invite) string {
	switch {
	case inv.MailFailed:
		return "Invite created for " + inv.Email + ", but the email could not be sent. Use Copy invite on their row to send the link yourself."
	case h.mail.Enabled():
		return "Invite emailed to " + inv.Email + ". Copy invite on their row has the link if you want to send it yourself."
	default:
		return "Invite created for " + inv.Email + ". This server sends no mail, so use Copy invite on their row and send the link yourself."
	}
}

// POST /orgs/:id/members/:userID/role
func (h *handler) SetMemberRole(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.ownedOrg(c)
	if err != nil {
		return err
	}
	back := backTo(c, o, "/orgs/"+o.Slug+"/settings/members")
	userID := c.Param("userID")
	role := c.FormValue("role")
	if role != "owner" && role != "member" && role != "viewer" {
		return echo.NewHTTPError(http.StatusBadRequest, "bad role")
	}
	m, err := h.store.GetOrgMember(ctx, o.ID, userID)
	if err != nil || m == nil {
		return echo.NewHTTPError(http.StatusNotFound, "member not found")
	}
	if m.Role == "owner" && role != "owner" && h.lastOwner(ctx, o.ID, userID) {
		middleware.SetFlash(c, "An org needs at least one owner.", middleware.FlashError)
		return respond.Redirect(c, back)
	}
	m.Role = role
	if err := h.store.UpsertOrgMember(ctx, m); err != nil {
		return err
	}
	middleware.SetFlash(c, "Role updated.", middleware.FlashSuccess)
	return respond.Redirect(c, back)
}

// POST /orgs/:id/members/:userID/remove
func (h *handler) RemoveMember(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.ownedOrg(c)
	if err != nil {
		return err
	}
	back := backTo(c, o, "/orgs/"+o.Slug+"/settings/members")
	userID := c.Param("userID")
	if m, err := h.store.GetOrgMember(ctx, o.ID, userID); err == nil && m != nil &&
		m.Role == "owner" && h.lastOwner(ctx, o.ID, userID) {
		middleware.SetFlash(c, "An org needs at least one owner.", middleware.FlashError)
		return respond.Redirect(c, back)
	}
	if err := h.store.DeleteOrgMember(ctx, o.ID, userID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Member removed.", middleware.FlashSuccess)
	return respond.Redirect(c, back)
}

// isSelf reports whether a people row is the viewer's own. Their row still
// shows, it just carries no role control and no Remove: the server refuses to
// strip the last owner anyway, and a button whose only outcomes are "no" and
// "you just locked yourself out" is not a button worth offering.
func isSelf(c echo.Context, userID string) bool {
	u := stackrmw.CurrentUser(c)
	return u != nil && u.ID == userID
}

// lastOwner reports whether userID is the org's only owner.
func (h *handler) lastOwner(ctx context.Context, orgID, userID string) bool {
	members, err := h.store.ListOrgMembers(ctx, orgID)
	if err != nil {
		return true // fail safe: refuse the demotion
	}
	for _, m := range members {
		if m.Role == "owner" && m.UserID != userID {
			return false
		}
	}
	return true
}

// POST /orgs/:id/invites, create a shareable invite link (14-day expiry).
func (h *handler) CreateInvite(c echo.Context) error {
	o, err := h.ownedOrg(c)
	if err != nil {
		return err
	}
	role := c.FormValue("role")
	if role != "owner" && role != "member" && role != "viewer" {
		role = "member"
	}
	inv, err := h.newInvite(c, o, strings.TrimSpace(strings.ToLower(c.FormValue("email"))), role, inviteExpiryDays(c))
	if err != nil {
		return err
	}
	middleware.SetFlash(c, h.inviteFlash(c, o, inv), middleware.FlashSuccess)
	return respond.Redirect(c, backTo(c, o, "/orgs/"+o.Slug+"/settings/members"))
}

// ownedInvite loads one of this org's invites, for an owner only.
func (h *handler) ownedInvite(c echo.Context) (*repo.Org, *repo.Invite, error) {
	o, err := h.ownedOrg(c)
	if err != nil {
		return nil, nil, err
	}
	inv, err := h.store.GetInvite(c.Request().Context(), c.Param("inviteID"))
	if err != nil || inv == nil || inv.OrgID != o.ID {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "invite not found")
	}
	return o, inv, nil
}

// POST /orgs/:id/invites/:inviteID/delete
func (h *handler) DeleteInvite(c echo.Context) error {
	o, inv, err := h.ownedInvite(c)
	if err != nil {
		return err
	}
	if err := h.store.DeleteInvite(c.Request().Context(), inv.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Invite revoked.", middleware.FlashSuccess)
	return respond.Redirect(c, backTo(c, o, "/orgs/"+o.Slug+"/settings/members"))
}

// POST /orgs/:id/invites/:inviteID/resend, mails the same token again. The
// token is the row id, so nothing already handed out stops working.
func (h *handler) ResendInvite(c echo.Context) error {
	o, inv, err := h.ownedInvite(c)
	if err != nil {
		return err
	}
	if !h.mail.Enabled() {
		return echo.NewHTTPError(http.StatusConflict, "this server sends no mail")
	}
	if inv.ExpiresAt.Before(time.Now().UTC()) {
		return echo.NewHTTPError(http.StatusConflict, "this invite has expired")
	}
	h.mailInvite(c, o, inv)
	if inv.MailFailed {
		middleware.SetFlash(c, "The email could not be sent. Copy the link and send it yourself.", middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Invite emailed again to "+inv.Email+".", middleware.FlashSuccess)
	}
	return respond.Redirect(c, backTo(c, o, "/orgs/"+o.Slug+"/settings/members"))
}

// POST /orgs/:id/invites/:inviteID/reinvite, the invite ran out, so mint a
// fresh token for the same address and role. New row first: the old one is only
// dropped once the replacement exists, so a failure here leaves the address
// invited rather than silently dropped.
func (h *handler) ReinviteMember(c echo.Context) error {
	o, inv, err := h.ownedInvite(c)
	if err != nil {
		return err
	}
	fresh, err := h.newInvite(c, o, inv.Email, inv.Role, inviteExpiryDays(c))
	if err != nil {
		return err
	}
	if err := h.store.DeleteInvite(c.Request().Context(), inv.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, h.inviteFlash(c, o, fresh), middleware.FlashSuccess)
	return respond.Redirect(c, backTo(c, o, "/orgs/"+o.Slug+"/settings/members"))
}

// personRow is one line of the People list: either a member or an invite nobody
// has accepted yet. Presentation only, the two live in different tables and
// only the page cares that they read alike.
type personRow struct {
	Name       string
	Email      string
	Role       string
	AvatarPath string
	UserID     string       // members only
	Invite     *repo.Invite // pending and expired rows only
	State      string       // member | pending | expired
}

// peopleRows merges members and open invites into the one list the People tab
// and wizard step 5 both render: members first, then invites newest first, the
// order the store already returns them in.
func peopleRows(members []repo.OrgMember, invites []repo.Invite) []personRow {
	now := time.Now().UTC()
	rows := make([]personRow, 0, len(members)+len(invites))
	for _, m := range members {
		rows = append(rows, personRow{
			Name: m.Name, Email: m.Email, Role: m.Role,
			AvatarPath: m.AvatarPath, UserID: m.UserID, State: "member",
		})
	}
	for i := range invites {
		state := "pending"
		if invites[i].ExpiresAt.Before(now) {
			state = "expired"
		}
		rows = append(rows, personRow{
			Name: invites[i].Email, Email: invites[i].Email, Role: invites[i].Role,
			Invite: &invites[i], State: state,
		})
	}
	return rows
}

// inviteURL builds the absolute join link for an invite token. Behind the proxy
// the request itself is plain HTTP, so the scheme comes from the forwarded
// header, get it wrong and every copied link is unusable.
func inviteURL(c echo.Context, token string) string {
	scheme := "https"
	if c.Request().TLS == nil && c.Request().Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return scheme + "://" + c.Request().Host + "/invite/" + token
}
