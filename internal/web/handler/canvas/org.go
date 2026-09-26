package canvas

import (
	"context"
	"slices"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
)

func day(t time.Time) string { return t.Format("2 Jan 2006") }

// orgTab is one verb read per tab; members needs member.list.
func (h *handler) orgTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, og := c.Request().Context(), cd.s.Org
	switch f.Tab {
	case "members":
		if !can(c, cd.s, "member.list") {
			f.Error = "Your role cannot list members."
			return templ.NopComponent, nil
		}
		v, err := h.membersView(c, cd, f.Base)
		return orgui.Members(v), err
	case "keys":
		ks, err := h.orch.Keys(ctx, middleware.Principal(c).User.ID)
		v := orgui.KeysView{Base: f.Base}
		for _, k := range ks {
			if k.OrgID != nil && *k.OrgID == og.ID {
				v.Keys = append(v.Keys, orgui.KeyRow{ID: k.ID, Name: k.Name, Created: day(k.CreatedAt)})
			}
		}
		return orgui.Keys(v), err
	case "params":
		return h.vars(c, cd.s, "", "")
	case "domains":
		return h.orgDomains(c, cd, f.Base)
	case "backups":
		return h.orgDests(c, cd, f.Base)
	case "config":
		return h.configTab(c, cd, f)
	}
	cascade, err := h.cascadeForm(c, cd, og.Settings)
	if err != nil {
		return nil, err
	}
	if !can(c, cd.s, "orgdefaults.set") {
		cascade.ReadOnly, cascade.Why = true, "Changing defaults needs an owner of this organization."
	}
	v := orgui.SettingsView{Name: og.Name, Slug: og.Slug, Cascade: cascade}
	if !can(c, cd.s, "org.write") {
		return orgui.Settings(v), nil
	}
	sts, err := h.orch.Stacks(ctx, og.ID)
	if err != nil {
		return nil, err
	}
	v.Stacks = len(sts)
	v.Rename = f.Base + "/rename"
	v.Delete = dialog.DeleteOrg(og.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
	return orgui.Settings(v), nil
}

// orgDests is the org's own destinations and the install's shared ones,
// which it may use but never change.
func (h *handler) orgDests(c echo.Context, cd card, base string) (templ.Component, error) {
	ds, err := h.orch.BackupDests(c.Request().Context(), cd.s.Org.ID)
	if err != nil {
		return nil, err
	}
	write := can(c, cd.s, "destination.write")
	v := comp.DestsView{Scope: "organization", ListHelp: "This organization's own buckets, and the ones the server shares with every organization."}
	if write {
		v.Add = base + "/backups"
	}
	for _, d := range ds {
		r := comp.DestView{Name: d.Name, Bucket: d.Bucket, Endpoint: d.Endpoint}
		switch {
		case d.Kind == "local":
			r.Note = "local disk"
		case d.OrgID == nil:
			r.Note = "install-wide"
		case write:
			r.Remove = comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove " + d.Name + "?",
				Warning: "stackr forgets this destination. Archives already in the bucket stay there.",
				Action:  base + "/backups/" + d.ID + "/delete",
				Target:  "#" + comp.DrawerRoot,
			}
		}
		v.Rows = append(v.Rows, r)
	}
	return orgui.Backups(v), nil
}

// orgDomains is the org's domain resources and the instance's, which it
// names tiles under but never changes; an owner adds and deletes its own.
func (h *handler) orgDomains(c echo.Context, cd card, base string) (templ.Component, error) {
	rs, err := h.orch.DomainResources(c.Request().Context(), cd.s.Org.ID)
	if err != nil {
		return nil, err
	}
	owner := can(c, cd.s, "domain.resource")
	v := orgui.DomainsView{Example: "example.com"}
	if owner {
		v.Add = base + "/domains"
	}
	for _, r := range rs {
		row := orgui.DomainRow{
			Host:       r.Host,
			Level:      r.Level,
			IncludeEnv: r.IncludeEnvOnDefault,
			ACMEEmail:  r.ACMEEmail,
		}
		if owner && r.OrgID != nil {
			row.Delete = comp.ConfirmView{
				Button:  "Delete",
				Title:   "Remove \"" + r.Host + "\"?",
				Warning: "New auto hostnames stop nesting under it.",
				Action:  base + "/domains/" + r.ID + "/delete",
				Target:  "#" + comp.DrawerRoot,
				Quiet:   true,
			}
		}
		v.Rows = append(v.Rows, row)
	}
	// rows are the org's first, then the instance's
	own := slices.IndexFunc(rs, func(r service.DomainResource) bool { return r.OrgID != nil })
	switch {
	case own >= 0:
		v.Example = rs[own].Host
	case len(rs) > 0:
		v.Host = cd.s.Org.Slug + "." + rs[0].Host
	}
	return orgui.Domains(v), nil
}

// ponytail: emails come from the user list (one read); a verb that joins
// members to users replaces it.
func (h *handler) membersView(c echo.Context, cd card, base string) (orgui.MembersView, error) {
	ctx, id := c.Request().Context(), cd.s.Org.ID
	v := orgui.MembersView{
		Base:   base,
		Manage: can(c, cd.s, "member.manage"),
		Self:   middleware.Principal(c).User.ID,
		Roles:  h.orch.Roles(),
	}
	ms, err := h.orch.Members(ctx, id)
	if err != nil {
		return v, err
	}
	us, err := h.orch.Users(ctx)
	if err != nil {
		return v, err
	}
	byID := map[string]service.User{}
	for _, u := range us {
		byID[u.ID] = u
	}
	for _, m := range ms {
		u := byID[m.UserID]
		v.Members = append(v.Members, orgui.MemberRow{UserID: m.UserID, Name: u.Name, Email: u.Email, Role: m.Role})
	}
	if !v.Manage {
		return v, nil
	}
	is, err := h.orch.PendingInvites(ctx, id)
	for _, i := range is {
		v.Invites = append(v.Invites, orgui.InviteRow{Email: i.Email, Role: i.Role, Expires: i.ExpiresAt.Format("Jan 2 2006")})
	}
	return v, err
}

// orgAction is a POST on the org drawer: one verb, then its tab again.
func (h *handler) orgAction(tab string, do func(echo.Context, *service.Org) (string, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		cd, _ := h.cardOf(c, "org")
		note, err := do(c, cd.s.Org)
		if err == nil && c.Response().Committed {
			return nil
		}
		return h.after(c, cd, tab, note, err)
	}
}

func (h *handler) mountOrg(site *echo.Group, a *middleware.Access) {
	o := "/:org/-/drawer"
	site.GET(o, h.drawerRoute("org"), a.Require("org.read"))
	owner, manage := a.Require("org.write"), a.Require("member.manage")
	site.POST(o+"/rename", h.orgAction("settings", func(c echo.Context, og *service.Org) (string, error) {
		n, err := h.orch.RenameOrg(c.Request().Context(), og.ID, c.FormValue("name"))
		if err != nil {
			return "", err
		}
		return redirect(c, "/?drawer=org:"+n.ID+"&tab=settings") // org cards are on home
	}), owner)
	site.POST(o+"/delete", h.orgAction("settings", func(c echo.Context, og *service.Org) (string, error) {
		if err := h.orch.DeleteOrg(c.Request().Context(), og.ID); err != nil {
			return "", err
		}
		return redirect(c, "/")
	}), owner)
	site.POST(o+"/invite", h.orgAction("members", func(c echo.Context, og *service.Org) (string, error) {
		i, err := h.orch.Invite(
			c.Request().Context(),
			og.ID,
			c.FormValue("email"),
			c.FormValue("role"),
			middleware.Principal(c).User.ID,
		)
		return "Invite link: /invite/" + i.ID, err
	}), manage)
	site.POST(o+"/members/:user/role", h.orgAction("members", func(c echo.Context, og *service.Org) (string, error) {
		return "Role changed.", h.orch.SetRole(c.Request().Context(), og.ID, c.Param("user"), c.FormValue("role"))
	}), manage)
	site.POST(o+"/members/:user/remove", h.orgAction("members", func(c echo.Context, og *service.Org) (string, error) {
		return "Member removed.", h.orch.RemoveMember(c.Request().Context(), og.ID, c.Param("user"))
	}), manage)
	site.POST(o+"/keys", h.orgAction("keys", func(c echo.Context, og *service.Org) (string, error) {
		tok, _, err := h.orch.MintKey(
			c.Request().Context(),
			middleware.Principal(c).User.ID,
			og.ID,
			c.FormValue("key_name"),
		)
		return "Copy it now, it is not shown again: " + tok, err
	}), a.Require("org.read"))
	site.POST(o+"/keys/:key/revoke", h.orgAction("keys", func(c echo.Context, _ *service.Org) (string, error) {
		return "Key revoked.", h.orch.RevokeKey(c.Request().Context(), middleware.Principal(c).User.ID, c.Param("key"))
	}), a.Require("org.read"))
	res := a.Require("domain.resource")
	site.POST(o+"/domains", h.orgAction("domains", func(c echo.Context, og *service.Org) (string, error) {
		r, err := h.orch.CreateDomainResource(
			c.Request().Context(),
			"org",
			og.ID,
			c.FormValue("host"),
			c.FormValue("include_env_on_default") != "",
			c.FormValue("acme_email"),
		)
		return "Domain " + r.Host + " added. This organization's tiles can now claim auto hostnames under it.", err
	}), res)
	site.POST(o+"/domains/:resource/delete", h.orgAction("domains", func(c echo.Context, _ *service.Org) (string, error) {
		return "Domain resource removed.", h.orch.DeleteDomainResource(c.Request().Context(), c.Param("resource"))
	}), res)
	site.POST(o+"/backups", h.orgAction("backups", func(c echo.Context, og *service.Org) (string, error) {
		f := c.FormValue
		_, err := h.orch.CreateBackupDest(c.Request().Context(), &og.ID, service.BackupDestSpec{
			Name:      f("dest_name"),
			Endpoint:  f("endpoint"),
			Region:    f("region"),
			Bucket:    f("bucket"),
			AccessKey: f("access_key"),
			SecretKey: f("secret_key"),
		})
		return "Destination added.", err
	}), a.Require("destination.write"))
	site.POST(o+"/backups/:dest/delete", h.orgAction("backups", func(c echo.Context, og *service.Org) (string, error) {
		return "Destination deleted.", h.orch.DeleteBackupDest(c.Request().Context(), og.ID, c.Param("dest"))
	}), a.Require("destination.write"))
	h.mountOrgConfig(site, a)
	site.POST(o+"/settings", h.saveRung(
		"org",
		func(s service.Scope) string { return s.Org.Settings },
		func(ctx context.Context, s service.Scope, blob string) error {
			_, err := h.orch.SetOrgSettings(ctx, s.Org.ID, blob)
			return err
		},
	), a.Require("orgdefaults.set"))
}
