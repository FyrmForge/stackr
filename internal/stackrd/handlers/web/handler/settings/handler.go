// Package settings owns the admin area at /admin: everything scoped to this
// installation, users, registries, TLS, notification
// defaults and maintenance.
//
// It used to serve one /settings page that mixed three different scopes:
// per-org connectors, instance infrastructure and the signed-in user's own API
// keys. Personal settings now live under /account and org settings on the
// org's own page, so every section here reaches exactly as far as the admin
// badge on it says.
package settings

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/imagewatch"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store        repo.Store
	backups      *backup.Service
	rt           *runtime.Runtime
	px           *proxy.Proxy
	gh           *githubapp.Client
	signer       *registry.Signer
	dataDir      string
	registryPort string
	// baseURL is the panel's own origin, which is what the registry advertises
	// as its token realm: a docker client has to be able to reach it.
	baseURL   string
	acmeEmail string
}

// NewHandler creates a new admin settings handler.
func NewHandler(store repo.Store, bk *backup.Service, rt *runtime.Runtime, px *proxy.Proxy, gh *githubapp.Client, signer *registry.Signer, dataDir, registryPort, acmeEmail, baseURL string) *handler {
	return &handler{store: store, backups: bk, rt: rt, px: px, gh: gh, signer: signer, dataDir: dataDir,
		registryPort: registryPort, acmeEmail: acmeEmail, baseURL: baseURL}
}

// GET /admin, users is the landing section.
func (h *handler) Index(c echo.Context) error {
	return respond.Redirect(c, "/admin/users")
}

// GET /settings, the old catch-all. Admins land in the admin area, everyone
// else in their own account, which is where the part of that page they could
// actually use (API keys) now lives.
func (h *handler) LegacyRedirect(c echo.Context) error {
	if stackrmw.IsAdmin(c) {
		return respond.Redirect(c, "/admin/users")
	}
	return respond.Redirect(c, "/account/profile")
}

// GET /admin/users
func (h *handler) Users(c echo.Context) error {
	users, err := h.store.ListUsers(c.Request().Context())
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, usersPage(c, users))
}

// GET /admin/registries
func (h *handler) Registries(c echo.Context) error {
	regs, err := h.store.ListRegistries(c.Request().Context())
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, registriesPage(c, regs))
}

// GET /admin/tls
func (h *handler) TLS(c echo.Context) error {
	ctx := c.Request().Context()
	dnsProvider, _ := h.store.GetSetting(ctx, "dns_provider")
	dnsEnv, _ := h.store.GetSetting(ctx, "dns_env")
	managed, err := h.store.GetManagedRegistry(ctx)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, tlsPage(c, dnsProvider, dnsEnv, h.acmeEmail, managed))
}

// GET /admin/maintenance
func (h *handler) Maintenance(c echo.Context) error {
	cleanup, _ := h.store.GetSetting(c.Request().Context(), "cleanup_enabled")
	interval, _ := h.store.GetSetting(c.Request().Context(), imagewatch.SettingInterval)
	if strings.TrimSpace(interval) == "" {
		interval = "5"
	}
	return respond.HTML(c, http.StatusOK, maintenancePage(c, cleanup == "1", interval))
}

// POST /admin/imagewatch, set the registry check cadence (minutes, 0 = off).
func (h *handler) SaveImageWatch(c echo.Context) error {
	v := strings.TrimSpace(c.FormValue("interval_minutes"))
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		middleware.SetFlash(c, "Check interval must be a non-negative number of minutes.", middleware.FlashError)
		return respond.Redirect(c, "/admin/maintenance")
	}
	if err := h.store.SetSetting(c.Request().Context(), imagewatch.SettingInterval, strconv.Itoa(n)); err != nil {
		return err
	}
	if n == 0 {
		middleware.SetFlash(c, "Image version checks disabled.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, fmt.Sprintf("Image versions checked every %d min.", n), middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/admin/maintenance")
}

// --- backup destinations ---
//
// Destinations exist at two scopes: an org's own, managed on that org's
// settings page, and server-wide ones, managed here and usable from every org.
// Both scopes render the same section and post to the same two handlers, the
// org id in the path is what tells them apart.

// destOrg resolves the org a destination request is scoped to: the :id path
// param on the org route, empty on the admin route (= server-wide). It also
// checks membership, because the org route is not behind adminOnly.
func (h *handler) destOrg(c echo.Context) (string, error) {
	orgID := orgParam(c)
	if orgID == "" {
		return "", nil // /admin/..., already behind adminOnly
	}
	o, err := h.store.GetOrgBySlug(c.Request().Context(), orgID)
	if err != nil {
		return "", err
	}
	if o != nil {
		orgID = o.ID
	}
	// Write rights in *this* org, not the cookie-selected one: a destination
	// holds bucket credentials, and deleting one cascades away every schedule
	// attached to it.
	if err := stackrmw.RequireOrgWrite(c, h.store, orgID); err != nil {
		return "", err
	}
	return orgID, nil
}

// ListDestinations returns the destinations visible at one scope: an org's own
// for an org page, the server-wide ones for the admin page.
func ListDestinations(ctx context.Context, store repo.Store, orgID string) ([]repo.BackupDestination, error) {
	all, err := store.ListBackupDestinations(ctx)
	if err != nil {
		return nil, err
	}
	var out []repo.BackupDestination
	for _, d := range all {
		if (orgID == "" && d.Global()) || (orgID != "" && d.OrgID.String == orgID) {
			out = append(out, d)
		}
	}
	return out, nil
}

// GET /admin/backups
func (h *handler) Backups(c echo.Context) error {
	ctx := c.Request().Context()
	ds, err := ListDestinations(ctx, h.store, "")
	if err != nil {
		return err
	}
	panelB, runs, err := h.panelBackup(ctx)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, backupsPage(c, ds, panelB, runs))
}

// panelBackup finds the schedule for stackr's own database. There is at most
// one, it is the server's, not a tile's, so it is not created per anything.
func (h *handler) panelBackup(ctx context.Context) (*repo.Backup, []repo.BackupRun, error) {
	all, err := h.store.ListBackups(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range all {
		if all[i].Kind == repo.BackupStackr {
			runs, _ := h.store.ListBackupRuns(ctx, all[i].ID, 10)
			return &all[i], runs, nil
		}
	}
	return nil, nil, nil
}

// POST /admin/backups/panel, create or update the schedule for stackr's own
// database. It may only write to a server-wide destination: the panel database
// holds every org's data, so putting it in one org's bucket hands them the
// rest.
func (h *handler) SavePanelBackup(c echo.Context) error {
	ctx := c.Request().Context()
	dest, err := h.store.GetBackupDestination(ctx, c.FormValue("destination_id"))
	if err != nil {
		return err
	}
	if dest == nil || !dest.Global() {
		middleware.SetFlash(c, "Pick a server-wide destination.", middleware.FlashError)
		return respond.Redirect(c, "/admin/backups")
	}
	b, _, err := h.panelBackup(ctx)
	if err != nil {
		return err
	}
	create := b == nil
	if create {
		b = &repo.Backup{ID: uuid.New().String(), Kind: repo.BackupStackr, CreatedAt: time.Now().UTC()}
	}
	b.DestinationID = dest.ID
	b.Cron = strings.TrimSpace(c.FormValue("cron"))
	b.Timezone = strings.TrimSpace(c.FormValue("timezone"))
	if n, convErr := strconv.Atoi(c.FormValue("keep_latest")); convErr == nil && n >= 0 {
		b.KeepLatest = n
	}
	b.Enabled = c.FormValue("enabled") != ""
	if err := backup.Validate(b); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return respond.Redirect(c, "/admin/backups")
	}
	if create {
		err = h.store.CreateBackup(ctx, b)
	} else {
		err = h.store.UpdateBackup(ctx, b)
	}
	if err != nil {
		return err
	}
	if h.backups != nil {
		_ = h.backups.LoadSchedules(ctx)
	}
	middleware.SetFlash(c, "Panel database backup saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/backups")
}

// POST /admin/backups/panel/run
func (h *handler) RunPanelBackup(c echo.Context) error {
	ctx := c.Request().Context()
	b, _, err := h.panelBackup(ctx)
	if err != nil || b == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no panel backup configured")
	}
	if _, err := h.backups.Run(ctx, b.ID, "manual"); err != nil {
		middleware.SetFlash(c, "Backup failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Panel database backed up.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/admin/backups")
}

// POST /admin/backups/panel/delete
func (h *handler) DeletePanelBackup(c echo.Context) error {
	ctx := c.Request().Context()
	b, _, err := h.panelBackup(ctx)
	if err != nil || b == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no panel backup configured")
	}
	if err := h.store.DeleteBackup(ctx, b.ID); err != nil {
		return err
	}
	if h.backups != nil {
		_ = h.backups.LoadSchedules(ctx)
	}
	middleware.SetFlash(c, "Panel database backup removed.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/backups")
}

// CreateDestination adds one, at whichever scope the route implies. The
// credentials are proved before they are stored, a destination that cannot be
// written to is worse than no destination, because it looks configured.
// POST /admin/backups/destinations and POST /orgs/:id/settings/backups/destinations
func (h *handler) CreateDestination(c echo.Context) error {
	orgID, err := h.destOrg(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	d := &repo.BackupDestination{
		ID:        uuid.New().String(),
		Name:      strings.TrimSpace(c.FormValue("name")),
		Endpoint:  strings.TrimSpace(c.FormValue("endpoint")),
		Region:    strings.TrimSpace(c.FormValue("region")),
		Bucket:    strings.TrimSpace(c.FormValue("bucket")),
		AccessKey: c.FormValue("access_key"),
		SecretKey: c.FormValue("secret_key"),
		CreatedAt: time.Now().UTC(),
	}
	if orgID != "" {
		d.OrgID = sql.NullString{String: orgID, Valid: true}
	} else {
		// Server-wide: shared decides whether every org may write into it.
		// Off unless asked, because the bucket credentials go with it.
		d.Shared = c.FormValue("shared") != ""
	}
	if d.Name == "" || d.Endpoint == "" || d.Bucket == "" {
		middleware.SetFlash(c, "Name, endpoint and bucket are required.", middleware.FlashError)
		return h.destinationsDone(c, orgID)
	}
	if err := backup.TestDestination(ctx, d); err != nil {
		middleware.SetFlash(c, "Could not write to that bucket: "+err.Error(), middleware.FlashError)
		return h.destinationsDone(c, orgID)
	}
	if err := h.store.CreateBackupDestination(ctx, d); err != nil {
		return err
	}
	middleware.SetFlash(c, "Destination added and verified.", middleware.FlashSuccess)
	return h.destinationsDone(c, orgID)
}

// DeleteDestination removes one. Schedules pointing at it go with it (the
// foreign key cascades); archives already in the bucket are untouched.
func (h *handler) DeleteDestination(c echo.Context) error {
	orgID, err := h.destOrg(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	d, err := h.store.GetBackupDestination(ctx, c.Param("destID"))
	if err != nil {
		return err
	}
	// Scope check, not just existence: an org page may only delete that org's
	// destinations, and the admin page only server-wide ones.
	if d == nil || d.OrgID.String != orgID {
		return echo.NewHTTPError(http.StatusNotFound, "destination not found")
	}
	if err := h.store.DeleteBackupDestination(ctx, d.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Destination removed. Its schedules went with it; the archives in the bucket did not.", middleware.FlashSuccess)
	return h.destinationsDone(c, orgID)
}

func (h *handler) destinationsDone(c echo.Context, orgID string) error {
	if orgID == "" {
		return respond.Redirect(c, "/admin/backups")
	}
	o, _ := h.store.GetOrg(c.Request().Context(), orgID)
	if o == nil {
		return respond.Redirect(c, "/")
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/backups")
}

// POST /admin/dns, DNS-01 provider for wildcard certs. Traefik is recreated
// in the background when provider or credentials changed.
func (h *handler) SaveDNS(c echo.Context) error {
	ctx := c.Request().Context()
	if err := h.store.SetSetting(ctx, "dns_provider", strings.TrimSpace(c.FormValue("dns_provider"))); err != nil {
		return err
	}
	if err := h.store.SetSetting(ctx, "dns_env", c.FormValue("dns_env")); err != nil {
		return err
	}
	go func() {
		if err := h.px.EnsureTraefik(context.Background()); err != nil {
			slog.Error("traefik restart after dns settings failed", "error", err)
		}
	}()
	middleware.SetFlash(c, "DNS settings saved. Traefik restarts if they changed.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/tls")
}

// --- connectors (org-scoped; the UI for them lives on the org settings page) ---

// OrgConnectors is the per-org connector listing.
type OrgConnectors struct {
	Org   repo.Org
	Items []ConnectorView
}

// ConnectorView is one connector rendered on the org settings page.
type ConnectorView struct {
	ID        string
	Name      string
	Provider  string
	Slug      string // github app slug, "" while pending
	Connected bool
}

// LoadOrgConnectors lists connectors for one org.
func LoadOrgConnectors(ctx context.Context, store repo.Store, org repo.Org) (OrgConnectors, error) {
	cns, err := store.ListConnectorsByOrg(ctx, org.ID)
	if err != nil {
		return OrgConnectors{}, err
	}
	oc := OrgConnectors{Org: org}
	for _, cn := range cns {
		v := ConnectorView{ID: cn.ID, Name: cn.Name, Provider: cn.Provider}
		if cn.Provider == "github" {
			cfg := githubapp.ParseConfig(cn.Config)
			v.Slug, v.Connected = cfg.Slug, cfg.Connected()
		}
		oc.Items = append(oc.Items, v)
	}
	return oc, nil
}

// POST /orgs/:id/settings/connectors/github, start the manifest flow.
func (h *handler) GitHubConnect(c echo.Context) error {
	orgID := orgParam(c)
	if orgID == "" {
		orgID = c.FormValue("org_id")
	}
	if orgID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "org_id required")
	}
	// The path carries the slug; the connector row needs the id.
	if o, err := h.store.GetOrgBySlug(c.Request().Context(), orgID); err != nil {
		return err
	} else if o != nil {
		orgID = o.ID
	}
	// Was unguarded: the org id came straight off the form, so anyone could
	// create a connector inside an org they are not a member of. The delete
	// path was hardened for exactly this; the create path was missed.
	if err := stackrmw.RequireOrgWrite(c, h.store, orgID); err != nil {
		return err
	}
	action, manifest, err := h.gh.Begin(c.Request().Context(), orgID, strings.TrimSpace(c.FormValue("gh_org")))
	if err != nil {
		return err
	}
	// GitHub returns the user on a callback URL of its own, so "this started in
	// the onboarding wizard" has to survive the round trip out of band. Lax is
	// required: the return is a cross-site top-level GET, which Strict drops.
	if c.FormValue("setup") != "" {
		c.SetCookie(&http.Cookie{
			Name: stackrmw.SetupCookie, Value: "1", Path: "/",
			MaxAge: 30 * 60, HttpOnly: true, Secure: stackrmw.SecureCookie(c),
			SameSite: http.SameSiteLaxMode,
		})
	}
	return respond.HTML(c, http.StatusOK, githubRedirect(action, manifest))
}

// GET /settings/github/callback, GitHub redirects here after app creation.
// The path is registered with GitHub when the manifest is created, so it stays
// put even though the rest of /settings moved.
func (h *handler) GitHubCallback(c echo.Context) error {
	ctx := c.Request().Context()
	cn, err := h.gh.Complete(ctx, c.QueryParam("code"), c.QueryParam("state"))
	if err != nil {
		middleware.SetFlash(c, "GitHub connection failed: "+err.Error(), middleware.FlashError)
		return respond.Redirect(c, "/")
	}
	middleware.SetFlash(c, "GitHub App created. Now install it on the repos you want to deploy.", middleware.FlashSuccess)
	org, _ := h.store.GetOrg(ctx, cn.OrgID)
	if org == nil {
		return respond.Redirect(c, "/")
	}
	if h.inSetup(c) {
		// Back to step 2, not on to step 3: the app exists but is installed
		// nowhere yet, and step 2 is what makes the user finish that.
		return respond.Redirect(c, "/orgs/"+org.Slug+"/setup/connector")
	}
	return respond.Redirect(c, "/orgs/"+org.Slug+"/settings/connectors")
}

// inSetup consumes the marker GitHubConnect left before sending the browser to
// github.com. Reading it clears it, so a later connector added from settings
// does not get dragged back into the wizard.
func (h *handler) inSetup(c echo.Context) bool {
	ck, err := c.Cookie(stackrmw.SetupCookie)
	if err != nil || ck.Value == "" {
		return false
	}
	c.SetCookie(&http.Cookie{Name: stackrmw.SetupCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: stackrmw.SecureCookie(c), SameSite: http.SameSiteLaxMode})
	return true
}

// POST /orgs/:id/settings/connectors/:connectorID/delete
func (h *handler) DeleteConnector(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("connectorID")
	if id == "" {
		id = c.Param("id")
	}
	cn, err := h.store.GetConnector(ctx, id)
	if err != nil {
		return err
	}
	if cn == nil {
		return echo.NewHTTPError(http.StatusNotFound, "connector not found")
	}
	// Was unguarded: any user could sever any org's connector (and its config
	// bindings) by ID.
	if err := stackrmw.RequireOrgWrite(c, h.store, cn.OrgID); err != nil {
		return err
	}
	if err := h.store.DeleteConnector(ctx, cn.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Connector removed. Delete the GitHub App itself at github.com/settings/apps if you no longer need it.", middleware.FlashSuccess)
	org, _ := h.store.GetOrg(ctx, cn.OrgID)
	if org == nil {
		return respond.Redirect(c, "/")
	}
	return respond.Redirect(c, "/orgs/"+org.Slug+"/settings/connectors")
}

// POST /admin/users/:id/admin, grant or revoke server-admin rights.
func (h *handler) ToggleUserAdmin(c echo.Context) error {
	ctx := c.Request().Context()
	u, err := h.store.GetUserByID(ctx, c.Param("id"))
	if err != nil || u == nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	if u.Role == "admin" {
		// Demotion: never remove the last admin.
		users, err := h.store.ListUsers(ctx)
		if err != nil {
			return err
		}
		admins := 0
		for _, x := range users {
			if x.Role == "admin" {
				admins++
			}
		}
		if admins <= 1 {
			middleware.SetFlash(c, "The server needs at least one admin.", middleware.FlashError)
			return respond.Redirect(c, "/admin/users")
		}
		u.Role = "user"
	} else {
		u.Role = "admin"
	}
	u.UpdatedAt = time.Now().UTC()
	if err := h.store.UpdateUser(ctx, u); err != nil {
		return err
	}
	middleware.SetFlash(c, "User updated.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/users")
}

// POST /admin/users/:id/toggle, enable or disable a user account.
func (h *handler) ToggleUserActive(c echo.Context) error {
	ctx := c.Request().Context()
	u, err := h.store.GetUserByID(ctx, c.Param("id"))
	if err != nil || u == nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	if u.Role == "admin" {
		middleware.SetFlash(c, "The server admin cannot be disabled.", middleware.FlashError)
		return respond.Redirect(c, "/admin/users")
	}
	u.Active = !u.Active
	u.UpdatedAt = time.Now().UTC()
	if err := h.store.UpdateUser(ctx, u); err != nil {
		return err
	}
	middleware.SetFlash(c, "User updated.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/users")
}

// POST /admin/cleanup, toggle nightly docker prune.
func (h *handler) ToggleCleanup(c echo.Context) error {
	v := "0"
	if c.FormValue("enabled") != "" {
		v = "1"
	}
	if err := h.store.SetSetting(c.Request().Context(), "cleanup_enabled", v); err != nil {
		return err
	}
	// Every other save on this page confirms; this one used to answer silently.
	if v == "1" {
		middleware.SetFlash(c, "Nightly cleanup enabled.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Nightly cleanup disabled.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/admin/maintenance")
}

// POST /admin/registry/domain, set (or clear) the managed registry's TLS
// domain and write the Traefik route. The container is recreated so it carries
// the "registry" network alias Traefik routes to.
func (h *handler) SetRegistryDomain(c echo.Context) error {
	ctx := c.Request().Context()
	reg, err := h.store.GetManagedRegistry(ctx)
	if err != nil {
		return err
	}
	if reg == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "enable the managed registry first")
	}
	reg.Domain = strings.TrimSpace(c.FormValue("domain"))
	if err := h.store.UpdateRegistry(ctx, reg); err != nil {
		return err
	}
	if reg.Domain != "" {
		// The "registry" alias traefik dials lives in the service spec, so an
		// EnsureManaged is enough, no recreate, and no roll when it is
		// already there.
		if _, err := registry.EnsureManaged(ctx, h.store, h.rt, h.signer, h.dataDir, h.registryPort, h.baseURL); err != nil {
			return err
		}
	}
	if err := h.px.WriteRegistry(reg.Domain); err != nil {
		return err
	}
	if reg.Domain == "" {
		middleware.SetFlash(c, "Registry domain removed.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Registry reachable at https://"+reg.Domain+" once DNS points here.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/admin/tls")
}

// POST /admin/registries, add an external registry.
func (h *handler) CreateRegistry(c echo.Context) error {
	r := &repo.Registry{
		ID:        uuid.New().String(),
		Name:      strings.TrimSpace(c.FormValue("name")),
		URL:       strings.TrimSpace(c.FormValue("url")),
		Username:  c.FormValue("username"),
		Password:  c.FormValue("password"),
		CreatedAt: time.Now().UTC(),
	}
	if r.Name == "" || r.URL == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name and url required")
	}
	if err := h.store.CreateRegistry(c.Request().Context(), r); err != nil {
		return err
	}
	middleware.SetFlash(c, "Registry added.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/registries")
}

// POST /admin/registries/:id/delete
func (h *handler) DeleteRegistry(c echo.Context) error {
	if err := h.store.DeleteRegistry(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	middleware.SetFlash(c, "Registry removed.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/registries")
}

// orgParam is the org segment of an /orgs/... route. Named :slug there; the id
// fallback keeps any route that still names it :id working.
func orgParam(c echo.Context) string {
	if k := c.Param("slug"); k != "" {
		return k
	}
	return c.Param("id")
}

// ToggleDestinationShared offers a server-wide destination to every
// organization, or takes it back.
//
// Turning it off while an org still schedules against it is refused, and the
// refusal names the tiles: the alternative is a backup that starts failing at
// 3am for a reason nobody can see from the panel.
// POST /admin/backups/destinations/:destID/shared
func (h *handler) ToggleDestinationShared(c echo.Context) error {
	ctx := c.Request().Context()
	d, err := h.store.GetBackupDestination(ctx, c.Param("destID"))
	if err != nil || d == nil {
		return echo.NewHTTPError(http.StatusNotFound, "destination not found")
	}
	if !d.Global() {
		return echo.NewHTTPError(http.StatusBadRequest, "sharing applies to server-wide destinations only")
	}
	want := c.FormValue("shared") != ""
	if !want && d.Shared {
		if users := h.destinationUsers(ctx, d.ID); len(users) > 0 {
			middleware.SetFlash(c, "Still used by "+strings.Join(users, ", ")+". Move those schedules first.", middleware.FlashError)
			return respond.Redirect(c, "/admin/backups")
		}
	}
	d.Shared = want
	if err := h.store.UpdateBackupDestination(ctx, d); err != nil {
		return err
	}
	if want {
		middleware.SetFlash(c, "Shared with every organization.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "No longer shared.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/admin/backups")
}

// destinationUsers names the stacks whose tiles back up to this destination.
//
// walks every backup row. Tens of rows; add a store query if an
// install ever grows big enough to notice.
func (h *handler) destinationUsers(ctx context.Context, destID string) []string {
	bs, err := h.store.ListBackups(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for i := range bs {
		if bs[i].DestinationID != destID || !bs[i].TileID.Valid {
			continue // a NULL tile is the panel's own backup; admins own both ends
		}
		t, err := h.store.GetTile(ctx, bs[i].TileID.String)
		if err != nil || t == nil {
			continue
		}
		st, err := h.store.GetStack(ctx, t.StackID)
		if err != nil || st == nil {
			continue
		}
		name := st.Slug + "/" + t.Slug
		if !seen[name] {
			seen[name], out = true, append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// auditPageSize is how far back the admin listing goes in one page. Deep
// enough to answer "who read this today" without turning the page into a
// download of the whole table.
const auditPageSize = 200

// Audit is the server-wide secret activity log: every reveal, copy, read over
// the API, and every write. Admin-only, because it spans every organization.
// GET /admin/audit
func (h *handler) Audit(c echo.Context) error {
	ctx := c.Request().Context()
	actor, action := c.QueryParam("actor"), c.QueryParam("action")
	events, err := h.store.ListAllAuditEvents(ctx, actor, action, auditPageSize)
	if err != nil {
		return err
	}
	// The actor list comes from the unfiltered page, so picking one does not
	// remove every other name from the dropdown.
	all, err := h.store.ListAllAuditEvents(ctx, "", "", auditPageSize)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	f := auditFilter{Actor: actor, Action: action}
	for _, e := range all {
		if e.Actor == "" || seen[e.Actor] {
			continue
		}
		seen[e.Actor] = true
		f.Actors = append(f.Actors, e.Actor)
	}
	sort.Strings(f.Actors)
	return respond.HTML(c, http.StatusOK, auditPage(c, events, f))
}
