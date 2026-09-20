package org

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/htmx"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	settingspage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The organization settings page. This replaces the drawer that used to hang
// off the org's own canvas card: the same members, invites, rename and delete,
// plus the connectors that were stranded on the global settings page and the
// org variables that previously had no UI at all.

// settingsOrg loads the org named in the URL and checks the viewer belongs to
// it. Accepts a slug or an id so old id-based links keep working.
func (h *handler) settingsOrg(c echo.Context) (*repo.Org, error) {
	ctx := c.Request().Context()
	key := orgKey(c)
	o, err := h.orgs.Resolve(ctx, key)
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	return o, nil
}

// SettingsIndex sends /orgs/:id/settings to its first section.
func (h *handler) SettingsIndex(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/general")
}

// GET /orgs/:id/settings/general
func (h *handler) SettingsGeneral(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	stacks, err := h.stacks.ListForOrg(ctx, o.ID)
	if err != nil {
		return err
	}
	var others []repo.Org
	for _, x := range stackrmw.Orgs(c) {
		if x.ID != o.ID {
			others = append(others, x)
		}
	}
	return respond.HTML(c, http.StatusOK, orgGeneralPage(c, o, h.ownerOf(c, o.ID), len(stacks), others, h.envColorRows(ctx, o, stacks)))
}

// envColorRow is one slug's default colour on the General tab.
type envColorRow struct {
	Slug    string
	Current string // the org's own value, "" when it has none
	CSS     string // what the slug is drawn in today, first stack's ladder
	Source  string // org | default
}

// envColorRows is every static environment slug across the org's stacks, in
// order of first appearance, with the org's default for it.
func (h *handler) envColorRows(ctx context.Context, o *repo.Org, stacks []repo.Stack) []envColorRow {
	defaults := envcolor.OrgDefaults(o)
	var rows []envColorRow
	seen := map[string]bool{}
	for _, s := range stacks {
		envs, err := h.envs.ListForStack(ctx, s.ID)
		if err != nil {
			continue
		}
		// Org value only: a stack's own override is its own business here.
		for i := range envs {
			envs[i].Color = ""
		}
		colors := envcolor.Map(envs, o, false)
		for _, e := range envs {
			if e.Type == "ephemeral" || seen[e.Slug] {
				continue
			}
			seen[e.Slug] = true
			rows = append(rows, envColorRow{Slug: e.Slug, Current: defaults[e.Slug],
				CSS: colors[e.ID].CSS, Source: colors[e.ID].Source})
		}
	}
	return rows
}

// SaveEnvColor sets the org-wide default colour for one env slug.
// POST /orgs/:slug/settings/env-colors
func (h *handler) SaveEnvColor(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	if o.ConfigManaged() {
		return echo.NewHTTPError(http.StatusConflict, "this organization is managed by "+o.ConfigRepo+"; set defaults.env_colors in the org config file")
	}
	slug := repo.Slugify(c.FormValue("slug"))
	if slug == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "environment slug required")
	}
	v, ok := components.EnvColorFromForm(c.FormValue("color"), c.FormValue("custom"))
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "pick a palette colour or a #rrggbb value")
	}
	m := envcolor.OrgDefaults(o)
	if v == "" {
		delete(m, slug)
	} else {
		m[slug] = v
	}
	o.EnvColors = ""
	if len(m) > 0 {
		b, _ := json.Marshal(m)
		o.EnvColors = string(b)
	}
	if err := h.store.UpdateOrg(ctx, o); err != nil {
		return err
	}
	middleware.SetFlash(c, "Colour saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/general")
}

// GET /orgs/:id/settings/members, the People tab: members plus, for owners,
// the add panel and the open invite links (an invite URL is a credential, so
// only owners are shown them).
func (h *handler) SettingsMembers(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	members, err := h.members.ListMembers(ctx, o.ID)
	if err != nil {
		return err
	}
	isOwner := h.ownerOf(c, o.ID)
	var invites []repo.Invite
	if isOwner {
		if invites, err = h.members.ListInvites(ctx, o.ID); err != nil {
			return err
		}
	}
	return respond.HTML(c, http.StatusOK, orgMembersPage(c, o, peopleRows(members, invites), h.addCandidates(c, o.ID, members), h.mail.Enabled(), isOwner))
}

// GET /orgs/:id/settings/invites, the invites tab merged into People; the URL
// stays alive because links to it exist.
func (h *handler) SettingsInvites(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/members")
}

// addCandidates is the roster shown in the add panel: every account on the
// server that is not already in this org. Server admins only. An org owner who
// is not an admin gets nothing here on purpose, otherwise owning any org would
// be a way to enumerate every user on a shared server.
func (h *handler) addCandidates(c echo.Context, orgID string, members []repo.OrgMember) []repo.User {
	if !stackrmw.IsAdmin(c) {
		return nil
	}
	users, err := h.store.ListUsers(c.Request().Context())
	if err != nil {
		return nil
	}
	in := map[string]bool{}
	for _, m := range members {
		in[m.UserID] = true
	}
	var out []repo.User
	for _, u := range users {
		if !in[u.ID] && u.Active {
			out = append(out, u)
		}
	}
	return out
}

// ownedSettingsOrg is settingsOrg plus the owner check.
func (h *handler) ownedSettingsOrg(c echo.Context) (*repo.Org, error) {
	return h.settingsOrg(c)
}

// GET /orgs/:id/settings/connectors
func (h *handler) SettingsConnectors(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	conns, err := settingspage.LoadOrgConnectors(c.Request().Context(), h.store, *o)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, orgConnectorsPage(c, o, conns))
}

// GET /orgs/:id/settings/backups, this org's own backup destinations. The
// section and both its handlers ship with the admin package; only the scope
// differs.
func (h *handler) SettingsBackups(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	dests, err := settingspage.ListDestinations(c.Request().Context(), h.store, o.ID)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, orgBackupsPage(c, o, dests))
}

// GET /orgs/:id/settings/variables, the page, or just the editor when an
// in-page action swapped it.
func (h *handler) SettingsVariables(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	return h.renderVars(c, o)
}

// VarsPanel is the canvas drawer view of the org's variables editor, the
// vars/secrets cards open this instead of leaving the graph.
// GET /orgs/:id/settings/variables/panel
func (h *handler) VarsPanel(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	return h.renderVars(c, o)
}

// renderVars serves the org's variables on all three surfaces, canvas drawer,
// in-page editor swap, full settings page, picked by the request's htmx
// target. Mirrors renderStackVars in handler/project.
func (h *handler) renderVars(c echo.Context, o *repo.Org) error {
	vars, err := h.vars.List(c.Request().Context(), service.OrgVars(o.ID))
	if err != nil {
		return err
	}
	canWrite := stackrmw.CanWriteOrg(c, h.store, o.ID)
	if !canWrite {
		// Read-only members see names and the fact of a secret, never a value.
		for i := range vars {
			if vars[i].Secret {
				vars[i].Value = ""
			}
		}
	}
	class, editName, editValue := panelParams(c, vars)
	stackrmw.AuditPanelViews(c, h.store, vars, repo.OwnerOrg, o.ID)
	cfg := components.VarsEditCfg{
		PostURL:    "/orgs/" + o.Slug + "/settings/vars",
		RefScope:   "org",
		PanelURL:   "/orgs/" + o.Slug + "/settings/variables",
		ValueURL:   "/orgs/" + o.Slug + "/settings/variables/value",
		Class:      class,
		EditName:   editName,
		EditValue:  editValue,
		EditSecret: varSecret(vars, editName),
		RevealName: c.QueryParam("reveal"),
		Adding:     c.QueryParam("new") != "",
		CanWrite:   canWrite,
	}
	switch varsSurface(c) {
	case surfaceDrawer:
		cfg.PanelURL = "/orgs/" + o.Slug + "/settings/variables/panel"
		return respond.HTML(c, http.StatusOK, orgVarsPanel(c, o, filterVarsClass(vars, class), cfg))
	case surfaceEditor:
		return respond.HTML(c, http.StatusOK, components.VarsEditor(c, vars, cfg))
	}
	// newest 50, no paging, add paging when someone asks to scroll back.
	events, err := h.store.ListAuditEvents(c.Request().Context(), repo.OwnerOrg, o.ID, 50)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, orgVariablesPage(c, o, vars, cfg, events))
}

// varSecret reports whether the named row is a secret, so an edit round trip
// keeps the flag it was stored with.
func varSecret(vars []repo.Variable, name string) bool {
	for i := range vars {
		if vars[i].Name == name {
			return vars[i].Secret
		}
	}
	return false
}

// OrgVarValue is the org drawer's Copy endpoint, plaintext plus audit row.
// GET /orgs/:id/settings/variables/value?name=X
func (h *handler) OrgVarValue(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	vars, err := h.vars.List(c.Request().Context(), service.OrgVars(o.ID))
	if err != nil {
		return err
	}
	return stackrmw.AuditServeValue(c, h.store, vars, repo.OwnerOrg, o.ID, audit.Copy)
}

// panelParams / filterVarsClass mirror the stack drawer's helpers in
// handler/project, same drawer, same view state, different scope.

func panelParams(c echo.Context, vars []repo.Variable) (class, editName, editValue string) {
	class = c.QueryParam("class")
	if class == "" {
		class = c.FormValue("class")
	}
	editName = c.QueryParam("edit")
	for i := range vars {
		if vars[i].Name == editName {
			editValue = vars[i].Value
		}
	}
	return class, editName, editValue
}

func filterVarsClass(vars []repo.Variable, class string) []repo.Variable {
	if class != "plain" && class != "secret" {
		return vars
	}
	out := vars[:0]
	for _, v := range vars {
		if v.Secret == (class == "secret") {
			out = append(out, v)
		}
	}
	return out
}

// The three surfaces the variables editor renders on, told apart by the htmx
// target a request asks to swap. Mirrors handler/project.
const (
	surfaceDrawer = "drawer"
	surfaceEditor = "editor"
	surfacePage   = "page"
)

func varsSurface(c echo.Context) string {
	switch htmx.GetTarget(c.Request()) {
	case "drawer-body":
		return surfaceDrawer
	case "vars-editor":
		return surfaceEditor
	}
	return surfacePage
}

// inEditor reports whether a var post came from one of the htmx editor
// surfaces, which re-render in place instead of redirecting away.
func inEditor(c echo.Context) bool {
	return varsSurface(c) != surfacePage
}

// varNameRe mirrors the stack-variable rule so both scopes accept the same
// names, a reference like ${{ org.NAME }} has to survive shell expansion.
var varNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// POST /orgs/:id/settings/vars, create or update an org variable.
func (h *handler) SaveOrgVar(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	// Write rights in this org, not in whichever one the cookie selects.
	name := c.FormValue("name")
	if err := h.vars.Set(ctx, service.OrgVars(o.ID), []service.VarWrite{{
		Name: name, Value: components.VarValue(c), Secret: c.FormValue("secret") != "",
	}}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if inEditor(c) {
		return h.renderVars(c, o)
	}
	middleware.SetFlash(c, "Variable saved. Services referencing it pick it up on their next deploy.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/variables")
}

// POST /orgs/:id/settings/vars/delete
func (h *handler) DeleteOrgVar(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	// Write rights in this org, not in whichever one the cookie selects.
	if err := h.vars.Unset(ctx, service.OrgVars(o.ID),
		[]string{c.FormValue("name")}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if inEditor(c) {
		return h.renderVars(c, o)
	}
	middleware.SetFlash(c, "Variable deleted.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/variables")
}

// GET /orgs/:id/settings/domains, this org's domain resources.
func (h *handler) SettingsDomains(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	res, err := h.orgDomains(c, o.ID)
	if err != nil {
		return err
	}
	// Owner, not write: both buttons on this page are owner-level now (the
	// delete always was, the save was raised to match — decision #1 in
	// 06-points-18-20.md). Offering a member the form would be offering a
	// form that always answers 403.
	return respond.HTML(c, http.StatusOK, orgDomainsPage(c, o, res, stackrmw.IsOwnerOf(c, h.store, o.ID)))
}

// GET /orgs/:id/settings/storage, this org's network shares. Read-only: they
// come from the org file or the CLI.
func (h *handler) SettingsStorage(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	all, err := h.storage.ListAll(c.Request().Context())
	if err != nil {
		return err
	}
	var shares []repo.Storage
	for _, s := range all {
		if s.OrgID == o.ID {
			shares = append(shares, s)
		}
	}
	return respond.HTML(c, http.StatusOK, orgStoragePage(c, o, shares))
}

// orgDomains is this org's own domain resources. There is no owner-scoped
// query, so the filtering happens here.
func (h *handler) orgDomains(c echo.Context, orgID string) ([]repo.DomainResource, error) {
	all, err := h.resources.ListAll(c.Request().Context())
	if err != nil {
		return nil, err
	}
	var res []repo.DomainResource
	for _, r := range all {
		if r.Level == "org" && r.OwnerID == orgID {
			res = append(res, r)
		}
	}
	return res, nil
}

// POST /orgs/:id/settings/domains, add an org-level domain resource.
func (h *handler) SaveOrgDomain(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	// Write rights in this org, not in whichever one the cookie selects.
	host := strings.TrimSpace(c.FormValue("host"))
	// The config-managed refusal, the host shape, the taken check and the
	// anti-squat rule all live in the service now. Squat used to be checked
	// *here only*, which made it decorative: the same name could be claimed
	// from the API, at stack level, or one tile down.
	if _, err := h.resources.Create(ctx, "org", o.ID, host, service.ResourceOpts{
		IncludeEnvOnDefault: c.FormValue("include_env_on_default") != "",
		ACMEEmail:           c.FormValue("acme_email"),
	}); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Domain "+host+" added. This organization's tiles can now claim auto hostnames under it.", middleware.FlashSuccess)
	// The wizard asks for one domain; having it is the step done.
	if inSetup(c) {
		return respond.Redirect(c, "/orgs/"+o.Slug+"/setup/team")
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/domains")
}

// POST /orgs/:id/settings/domains/delete
func (h *handler) DeleteOrgDomain(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	r, err := h.resources.Get(ctx, c.FormValue("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	// Only this org's own rows; the id comes from a form field.
	if r.Level != "org" || r.OwnerID != o.ID {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := h.resources.Delete(ctx, r.ID); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Domain resource removed. Existing generated hostnames keep working until their tile redeploys.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/domains")
}

// inviteExpiryDays reads the expiry field, falling back to two weeks. It used
// to be hard-coded, which made long-lived invites impossible.
func inviteExpiryDays(c echo.Context) int {
	n, err := strconv.Atoi(c.FormValue("expires_days"))
	if err != nil || n < 1 || n > 365 {
		return 14
	}
	return n
}

// orgKey is the org segment of the URL. The routes name it :slug and the links
// build it from the slug, but loadOrg accepts an id too, so a bookmark from
// before the rename still resolves. The fallback covers routes that still name
// the param :id.
func orgKey(c echo.Context) string {
	if k := c.Param("slug"); k != "" {
		return k
	}
	return c.Param("id")
}
