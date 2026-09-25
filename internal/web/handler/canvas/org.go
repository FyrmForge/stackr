package canvas

import (
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
	case "backups":
		ds, err := h.orch.BackupDests(ctx, og.ID)
		v := orgui.BackupsView{Base: f.Base, Write: can(c, cd.s, "destination.write")}
		for _, d := range ds {
			v.Dests = append(v.Dests, orgui.DestRow{
				ID:     d.ID,
				Name:   d.Name,
				Kind:   d.Kind,
				Bucket: d.Bucket,
				Global: d.OrgID == nil,
			})
		}
		return orgui.Backups(v), err
	}
	v := orgui.SettingsView{Name: og.Name}
	if can(c, cd.s, "org.write") {
		v.Rename = f.Base + "/rename"
		v.Delete = dialog.DeleteOrg(og.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
	}
	return orgui.Settings(v), nil
}

// ponytail: emails come from the user list (one read); a verb that joins
// members to users replaces it.
func (h *handler) membersView(c echo.Context, cd card, base string) (orgui.MembersView, error) {
	ctx, id := c.Request().Context(), cd.s.Org.ID
	v := orgui.MembersView{Base: base, Manage: can(c, cd.s, "member.manage")}
	ms, err := h.orch.Members(ctx, id)
	if err != nil {
		return v, err
	}
	us, err := h.orch.Users(ctx)
	if err != nil {
		return v, err
	}
	email := map[string]string{}
	for _, u := range us {
		email[u.ID] = u.Email
	}
	for _, m := range ms {
		v.Members = append(v.Members, orgui.MemberRow{UserID: m.UserID, Email: email[m.UserID], Role: m.Role})
	}
	if !v.Manage {
		return v, nil
	}
	is, err := h.orch.PendingInvites(ctx, id)
	for _, i := range is {
		v.Invites = append(v.Invites, orgui.InviteRow{Email: i.Email, Role: i.Role, Expires: day(i.ExpiresAt)})
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
}
