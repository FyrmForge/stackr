package canvas

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
	placementui "github.com/FyrmForge/stackr/internal/ui/placement"
)

// panelURL is path on the panel's public address (comp.PanelURL with the
// panel_domain setting), so an invite link made after the panel moved does
// not name the old host.
func (h *handler) panelURL(ctx context.Context, path string) string {
	host, _ := h.orch.Setting(ctx, "panel_domain")
	return comp.PanelURL(host, path)
}

func day(t time.Time) string { return t.Format("2 Jan 2006") }

// mintedKey carries a just-minted token from the mint to the keys tab it
// answers with.
const mintedKey = "minted-key"

// openKey carries the accordion row an action belongs to, so the tab it
// answers with has that row open.
const openKey = "open-row"

// openRow is the accordion row open on load: ?open=, else the action's.
func openRow(c echo.Context) string {
	o, _ := c.Get(openKey).(string)
	return cmp.Or(c.FormValue("open"), o)
}

// orgTabs are the tabs the viewer's role can use; the rest are not drawn.
func orgTabs(c echo.Context, s service.Scope) []string {
	return slices.DeleteFunc(slices.Clone(orgui.Tabs), func(t string) bool {
		switch t {
		case "domains":
			return !can(c, s, "domain.resource")
		case "connectors":
			return !can(c, s, "connector.write") && !can(c, s, "connector.manage") && !can(c, s, "dns.manage")
		}
		return false
	})
}

// orgTab is one verb read per tab; a tab the role cannot use is hidden and
// asking for it opens General.
func (h *handler) orgTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, og := c.Request().Context(), cd.s.Org
	f.Tabs, f.Labels = orgTabs(c, cd.s), orgui.Labels
	if !slices.Contains(f.Tabs, f.Tab) {
		f.Tab = f.Tabs[0]
	}
	switch f.Tab {
	case "people":
		var m *orgui.MembersView
		if can(c, cd.s, "member.list") {
			v, err := h.membersView(c, cd, f.Base)
			if err != nil {
				return nil, err
			}
			m = &v
		}
		ks, err := h.orch.Keys(ctx, middleware.Principal(c).User.ID)
		v := orgui.KeysView{Base: f.Base}
		v.Minted, _ = c.Get(mintedKey).(string)
		for _, k := range ks {
			if k.OrgID != nil && *k.OrgID == og.ID {
				v.Keys = append(v.Keys, orgui.KeyRow{ID: k.ID, Name: k.Name, Created: day(k.CreatedAt), Role: k.Level, Stack: k.Stack})
			}
		}
		acc := middleware.Principal(c).Access
		have := authz.LevelAdmin
		if !acc.Admin {
			have, _ = authz.RoleLevel(acc.Roles[og.ID])
		}
		for _, r := range h.orch.Roles() {
			if l, _ := authz.RoleLevel(r); l <= have {
				v.Roles = append(v.Roles, r)
			}
		}
		if acc.Admin {
			v.Roles = append(v.Roles, "admin")
		}
		sts, serr := h.orch.Stacks(ctx, og.ID)
		for _, st := range sts {
			v.Stacks = append(v.Stacks, orgui.StackOpt{Slug: st.Slug, Name: st.Name})
		}
		err = errors.Join(err, serr)
		return orgui.People(m, v), err
	case "variables":
		return h.vars(c, cd.s, "", "")
	case "domains":
		return h.orgDomains(c, cd, f.Base)
	case "connectors":
		return h.connectorsTab(c, og)
	case "storage":
		v := orgui.StorageView{Open: openRow(c)}
		var err error
		if v.Registry, err = h.orgRegistry(c, cd); err != nil {
			return nil, err
		}
		if v.Registry.Repo != "" {
			v.Open = "registry"
		}
		if v.Shares, err = h.orgShares(c, cd, f.Base); err != nil {
			return nil, err
		}
		v.Dests, err = h.orgDests(c, cd, f.Base)
		return orgui.Storage(v), err
	case "config":
		return h.configTab(c, cd, f)
	case "placement":
		return placementui.Load(urlOf(cd.s) + "/-/placement"), nil
	}
	cascade, err := h.cascadeForm(c, cd, og.Settings)
	if err != nil {
		return nil, err
	}
	if !can(c, cd.s, "orgdefaults.set") {
		cascade.ReadOnly, cascade.Why = true, "Changing defaults needs an owner of this organization."
	}
	v := orgui.SettingsView{Name: og.Name, Slug: og.Slug, Cascade: cascade, Defaults: cascadeSummary(cascade.Rows), Open: openRow(c), Refresh: f.Base + "?tab=general"}
	if v.Tiers, err = h.tiersView(c, cd, f.Base); err != nil {
		return nil, err
	}
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

// tiersView is the ladder; an owner (tier.write) also gets the verbs' base.
// ponytail: stack counts walk every stack's envs; a count verb replaces it.
func (h *handler) tiersView(c echo.Context, cd card, base string) (orgui.TiersView, error) {
	ctx, og := c.Request().Context(), cd.s.Org
	v := orgui.TiersView{Managed: og.ConfigRepo != ""}
	ts, err := h.orch.Tiers(ctx, og.ID)
	if err != nil {
		return v, err
	}
	sts, err := h.orch.Stacks(ctx, og.ID)
	if err != nil {
		return v, err
	}
	in := map[string]int{}
	for _, st := range sts {
		es, err := h.orch.Envs(ctx, st.ID)
		if err != nil {
			return v, err
		}
		for _, e := range es {
			if e.Type == "static" {
				in[e.Slug]++
			}
		}
	}
	owner := can(c, cd.s, "tier.write")
	if owner {
		v.Base = base + "/tiers"
	}
	slugs := make([]string, len(ts))
	for i, t := range ts {
		slugs[i] = t.Slug
	}
	swap := func(i, j int) []string {
		if j < 0 || j >= len(slugs) {
			return nil
		}
		o := slices.Clone(slugs)
		o[i], o[j] = o[j], o[i]
		return o
	}
	for i, t := range ts {
		r := orgui.TierRow{Slug: t.Slug, Stacks: in[t.Slug], Locked: t.Locked, Up: swap(i, i-1), Down: swap(i, i+1)}
		if owner {
			r.Delete = comp.ConfirmView{
				Button:  "Delete",
				Title:   "Delete tier " + t.Slug + "?",
				Warning: "Refused while a stack env is still named " + t.Slug + ". Its tier params go with it.",
				Action:  v.Base + "/" + t.Slug + "/delete",
				Target:  "#" + comp.DrawerRoot,
				Quiet:   true,
			}
		}
		v.Rows = append(v.Rows, r)
	}
	return v, nil
}

// orgDests is the org's own destinations and the install's shared ones,
// which it may use but never change.
func (h *handler) orgDests(c echo.Context, cd card, base string) (comp.DestsView, error) {
	v := comp.DestsView{}
	ds, err := h.orch.BackupDests(c.Request().Context(), cd.s.Org.ID)
	if err != nil {
		return v, err
	}
	write := can(c, cd.s, "destination.write")
	v = comp.DestsView{Scope: "organization", ListHelp: "This organization's own buckets, and the ones the server shares with every organization."}
	if write {
		v.Add = base + "/backups"
	} else {
		v.Why = "Only members and owners can add a destination."
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
	return v, nil
}

// orgShares is the org's network shares; an owner adds and removes them.
func (h *handler) orgShares(c echo.Context, cd card, base string) (orgui.SharesView, error) {
	ss, err := h.orch.Shares(c.Request().Context(), cd.s.Org.ID)
	if err != nil {
		return orgui.SharesView{}, err
	}
	owner := can(c, cd.s, "share.write")
	v := orgui.SharesView{}
	if owner {
		v.Add = base + "/shares"
	}
	for _, s := range ss {
		row := orgui.ShareRow{
			Slug:     s.Slug,
			Kind:     s.Kind,
			Source:   s.Source,
			Options:  s.Options,
			User:     s.User,
			Password: s.PasswordRef,
		}
		if owner {
			row.Delete = comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove share \"" + s.Slug + "\"?",
				Warning: "The export is untouched. Refused while a tile mounts it.",
				Action:  base + "/shares/" + s.ID + "/delete",
				Target:  "#" + comp.DrawerRoot,
				Quiet:   true,
			}
		}
		v.Rows = append(v.Rows, row)
	}
	return v, nil
}

// orgDomains is the org's domain resources and the instance's, which it
// names tiles under but never changes; an owner adds and deletes its own.
func (h *handler) orgDomains(c echo.Context, cd card, base string) (templ.Component, error) {
	rs, err := h.orch.DomainResources(c.Request().Context(), cd.s.Org.ID)
	if err != nil {
		return nil, err
	}
	owner := can(c, cd.s, "domain.resource")
	v := orgui.DomainsView{Example: "example.com", NoWildcard: !h.orch.Wildcard(c.Request().Context())}
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
			row.Rename = base + "/domains/" + r.ID + "/rename"
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
		v.Invites = append(v.Invites, orgui.InviteRow{
			ID:      i.ID,
			Email:   i.Email,
			Role:    i.Role,
			Expires: i.ExpiresAt.Format("Jan 2 2006"),
			Link:    h.panelURL(ctx, "/invite/"+i.ID),
		})
	}
	return v, err
}

// orgAction is a POST on the org drawer: one verb, then its tab again.
// "tab/row" also opens that accordion row.
func (h *handler) orgAction(tab string, do func(echo.Context, *service.Org) (string, error)) echo.HandlerFunc {
	tab, row, _ := strings.Cut(tab, "/")
	return func(c echo.Context) error {
		c.Set(openKey, row)
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
	site.POST(o+"/rename", h.orgAction("general/name", func(c echo.Context, og *service.Org) (string, error) {
		n, err := h.orch.RenameOrg(c.Request().Context(), og.ID, c.FormValue("name"))
		if err != nil {
			return "", err
		}
		return renamed(c, n.Name, "/"+og.Slug, "/"+n.Slug, "/", "drawer=org:"+n.ID+"&tab=general&open=name") // org cards are on home
	}), owner)
	site.POST(o+"/delete", h.orgAction("general/delete", func(c echo.Context, og *service.Org) (string, error) {
		if err := h.orch.DeleteOrg(c.Request().Context(), og.ID); err != nil {
			return "", err
		}
		return redirect(c, "/")
	}), owner)
	site.POST(o+"/invite", h.orgAction("people", func(c echo.Context, og *service.Org) (string, error) {
		i, err := h.orch.Invite(
			c.Request().Context(),
			og.ID,
			c.FormValue("email"),
			c.FormValue("role"),
			middleware.Principal(c).User.ID,
		)
		return "Invite link: " + h.panelURL(c.Request().Context(), "/invite/"+i.ID), err
	}), manage)
	site.POST(o+"/invites/:invite/revoke", h.orgAction("people", func(c echo.Context, og *service.Org) (string, error) {
		return "Invite revoked.", h.orch.RevokeInvite(c.Request().Context(), og.ID, c.Param("invite"))
	}), manage)
	site.POST(o+"/members/:user/role", h.orgAction("people", func(c echo.Context, og *service.Org) (string, error) {
		return "Role changed.", h.orch.SetRole(c.Request().Context(), og.ID, c.Param("user"), c.FormValue("role"))
	}), manage)
	site.POST(o+"/members/:user/remove", h.orgAction("people", func(c echo.Context, og *service.Org) (string, error) {
		return "Member removed.", h.orch.RemoveMember(c.Request().Context(), og.ID, c.Param("user"))
	}), manage)
	site.POST(o+"/keys", h.orgAction("people", func(c echo.Context, og *service.Org) (string, error) {
		tok, _, err := h.orch.MintKey(
			c.Request().Context(),
			middleware.Principal(c),
			og.ID,
			cmp.Or(c.FormValue("name"), c.FormValue("key_name")),
			c.FormValue("level"),
			c.FormValue("stack"),
		)
		c.Set(mintedKey, tok) // the keys tab shows it once, with a Copy
		return "", err
	}), a.Require("org.read"))
	site.POST(o+"/keys/:key/revoke", h.orgAction("people", func(c echo.Context, _ *service.Org) (string, error) {
		return "Key revoked.", h.orch.RevokeKey(c.Request().Context(), middleware.Principal(c), c.Param("key"))
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
	site.POST(o+"/domains/:resource/rename", h.orgAction("domains", func(c echo.Context, _ *service.Org) (string, error) {
		r, err := h.orch.RenameDomainResource(c.Request().Context(), c.Param("resource"), c.FormValue("host"))
		return "Domain renamed to " + r.Host + ". Old names redirect until removed.", err
	}), res)
	shr := a.Require("share.write")
	site.POST(o+"/shares", h.orgAction("storage/shares", func(c echo.Context, og *service.Org) (string, error) {
		f := c.FormValue
		s, err := h.orch.CreateShare(c.Request().Context(), og.ID, service.ShareSpec{
			Slug:        f("slug"),
			Kind:        f("kind"),
			Source:      f("source"),
			Options:     f("options"),
			User:        f("user"),
			PasswordRef: f("password"),
		})
		return "Share " + s.Slug + " added.", err
	}), shr)
	site.POST(o+"/shares/:share/delete", h.orgAction("storage/shares", func(c echo.Context, og *service.Org) (string, error) {
		return "Share removed.", h.orch.DeleteShare(c.Request().Context(), og.ID, c.Param("share"))
	}), shr)
	site.POST(o+"/backups", h.orgAction("storage/backups", func(c echo.Context, og *service.Org) (string, error) {
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
	site.POST(o+"/backups/:dest/delete", h.orgAction("storage/backups", func(c echo.Context, og *service.Org) (string, error) {
		return "Destination deleted.", h.orch.DeleteBackupDest(c.Request().Context(), og.ID, c.Param("dest"))
	}), a.Require("destination.write"))
	tw := a.Require("tier.write")
	site.POST(o+"/tiers", h.orgAction("general/tiers", func(c echo.Context, og *service.Org) (string, error) {
		t, err := h.orch.CreateTier(c.Request().Context(), og.ID, c.FormValue("slug"))
		return "Tier " + t.Slug + " added.", err
	}), tw)
	site.POST(o+"/tiers/order", h.orgAction("general/tiers", func(c echo.Context, og *service.Org) (string, error) {
		form, _ := c.FormParams()
		return "Saved.", h.orch.ReorderTiers(c.Request().Context(), og.ID, form["slugs"])
	}), tw)
	site.POST(o+"/tiers/:tier/rename", h.orgAction("general/tiers", func(c echo.Context, og *service.Org) (string, error) {
		_, err := h.orch.RenameTier(c.Request().Context(), og.ID, c.Param("tier"), c.FormValue("name"))
		return "Tier renamed.", err
	}), tw)
	site.POST(o+"/tiers/:tier/delete", h.orgAction("general/tiers", func(c echo.Context, og *service.Org) (string, error) {
		return "Tier deleted.", h.orch.DeleteTier(c.Request().Context(), og.ID, c.Param("tier"))
	}), tw)
	site.POST(o+"/tiers/:tier/lock", h.orgAction("general/tiers", func(c echo.Context, og *service.Org) (string, error) {
		_, err := h.orch.SetTierLock(c.Request().Context(), og.ID, c.Param("tier"), c.FormValue("locked") != "")
		return "Saved.", err
	}), tw)
	h.mountOrgConfig(site, a)
	h.mountDNS(site, a)
	h.mountRegistry(site, a)
	site.POST(o+"/settings", h.saveRung(
		"org",
		func(s service.Scope) string { return s.Org.Settings },
		func(ctx context.Context, s service.Scope, blob string) error {
			_, err := h.orch.SetOrgSettings(ctx, s.Org.ID, blob)
			return err
		},
	), a.Require("orgdefaults.set"))
}
