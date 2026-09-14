package org

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/htmx"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	settingspage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
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
	o, err := h.store.GetOrgBySlug(ctx, key)
	if err != nil {
		return nil, err
	}
	if o == nil {
		if o, err = h.store.GetOrg(ctx, key); err != nil {
			return nil, err
		}
	}
	if o == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	if err := stackrmw.RequireOrgWrite(c, h.store, o.ID); err != nil {
		return nil, err
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
	stacks, err := h.store.ListStacksByOrg(ctx, o.ID)
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
		envs, err := h.store.ListEnvironmentsByStack(ctx, s.ID)
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
	members, err := h.store.ListOrgMembers(ctx, o.ID)
	if err != nil {
		return err
	}
	isOwner := h.ownerOf(c, o.ID)
	var invites []repo.Invite
	if isOwner {
		if invites, err = h.store.ListInvitesByOrg(ctx, o.ID); err != nil {
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
	o, err := h.settingsOrg(c)
	if err != nil {
		return nil, err
	}
	if !h.ownerOf(c, o.ID) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	return o, nil
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
	vars, err := h.store.ListVariables(c.Request().Context(), repo.OwnerOrg, o.ID)
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
	audit.PanelViews(c, h.store, vars, repo.OwnerOrg, o.ID)
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
	// Copy hands out plaintext, so it needs the same write rights that gate
	// the unmasked drawer view. Checked explicitly: RequireOrgWrite only
	// refuses mutating requests, and this is a GET.
	if !stackrmw.CanWriteOrg(c, h.store, o.ID) {
		return echo.NewHTTPError(http.StatusForbidden, "read-only")
	}
	vars, err := h.store.ListVariables(c.Request().Context(), repo.OwnerOrg, o.ID)
	if err != nil {
		return err
	}
	return audit.ServeValue(c, h.store, vars, repo.OwnerOrg, o.ID, audit.Copy)
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
	if err := stackrmw.RequireOrgWrite(c, h.store, o.ID); err != nil {
		return err
	}
	name := c.FormValue("name")
	if !varNameRe.MatchString(name) {
		return echo.NewHTTPError(http.StatusBadRequest, "variable name: letters, digits, _ . - only")
	}
	now := time.Now().UTC()
	v := &repo.Variable{
		OwnerKind: repo.OwnerOrg,
		OwnerID:   o.ID,
		Name:      name,
		Value:     components.VarValue(c),
		Secret:    c.FormValue("secret") != "",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := h.store.UpsertVariable(ctx, v); err != nil {
		return err
	}
	audit.Record(ctx, h.store, audit.Actor(c), audit.Set, repo.OwnerOrg, o.ID, name)
	deploy.ClearWaitingOrg(ctx, h.store, o.ID, name)
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
	if err := stackrmw.RequireOrgWrite(c, h.store, o.ID); err != nil {
		return err
	}
	if err := h.store.DeleteVariable(ctx, repo.OwnerOrg, o.ID, c.FormValue("name")); err != nil {
		return err
	}
	audit.Record(ctx, h.store, audit.Actor(c), audit.Delete, repo.OwnerOrg, o.ID, c.FormValue("name"))
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
	return respond.HTML(c, http.StatusOK, orgDomainsPage(c, o, res, stackrmw.CanWriteOrg(c, h.store, o.ID)))
}

// orgDomains is this org's own domain resources. There is no owner-scoped
// query, so the filtering happens here.
func (h *handler) orgDomains(c echo.Context, orgID string) ([]repo.DomainResource, error) {
	all, err := h.store.ListDomainResources(c.Request().Context())
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
	if err := stackrmw.RequireOrgWrite(c, h.store, o.ID); err != nil {
		return err
	}
	// stackr-org.yml models domains: now, so a managed org's are its file's.
	if o.ConfigManaged() {
		return echo.NewHTTPError(http.StatusConflict, "this organization is managed by "+o.ConfigRepo+"; declare domains: in the org config file")
	}
	host := strings.TrimSpace(c.FormValue("host"))
	if err := envops.ValidateResourceHost(host); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	all, err := h.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	if envops.HostTaken(all, host) {
		return echo.NewHTTPError(http.StatusConflict, "that host is already a domain resource")
	}
	if err := envops.CheckOrgSquat(ctx, h.store, host, o.ID); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	r := &repo.DomainResource{
		ID: uuid.New().String(), Level: "org", OwnerID: o.ID, Host: host,
		IncludeEnvOnDefault: c.FormValue("include_env_on_default") != "",
		CreatedAt:           time.Now().UTC(),
	}
	if err := h.store.CreateDomainResource(ctx, r); err != nil {
		return err
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
	if err := stackrmw.RequireOrgWrite(c, h.store, o.ID); err != nil {
		return err
	}
	if o.ConfigManaged() {
		return echo.NewHTTPError(http.StatusConflict, "this organization is managed by "+o.ConfigRepo+"; remove the host from domains: in the org config file")
	}
	// Only this org's own rows, the id comes from a form field.
	all, err := h.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.ID == c.FormValue("id") && r.Level == "org" && r.OwnerID == o.ID {
			if err := h.store.DeleteDomainResource(ctx, r.ID); err != nil {
				return err
			}
			middleware.SetFlash(c, "Domain resource removed. Existing generated hostnames keep working until their tile redeploys.", middleware.FlashSuccess)
			break
		}
	}
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
