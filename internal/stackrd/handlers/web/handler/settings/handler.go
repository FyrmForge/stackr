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
	"errors"
	"fmt"
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
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store   repo.Store
	backups *backup.Service
	// sched re-registers the cron and backup tables after a write that
	// changes or cascades their rows.
	sched *scheduler.Service
	// watch owns the registry check cadence and what an auto policy does.
	watch        *service.ImageWatchService
	admin        *service.AdminService
	rt           *runtime.Runtime
	px           *svcproxy.Service
	gh           *githubapp.Client
	signer       *registry.Signer
	dataDir      string
	registryPort string
	// baseURL is the panel's own origin, which is what the registry advertises
	// as its token realm: a docker client has to be able to reach it.
	baseURL   string
	acmeEmail string
	// registries owns the registry rows and the managed one's guards.
	registries *service.RegistryService
	// revoke closes what a live re-check cannot reach. Deactivating a user is
	// felt at their next request everywhere except the share links they
	// minted, which carry no user at all.
	revoke *service.RevokeService
	// dests owns the destination rules: the trim, the bucket probe, the
	// cascade's schedule reload and who still writes to a shared bucket.
	dests     *service.BackupDestinationService
	orgs      *service.OrgService
	settings  *service.SettingsService
	schedules *service.BackupScheduleService
	auth      *service.AuthService
	audit     *service.AuditService
}

// WithRegistries attaches the registry service.
func (h *handler) WithRegistries(r *service.RegistryService) *handler { h.registries = r; return h }

// WithRevoke attaches the revocation service.
func (h *handler) WithRevoke(r *service.RevokeService) *handler { h.revoke = r; return h }

// WithDestinations attaches the backup destination service.
func (h *handler) WithDestinations(d *service.BackupDestinationService) *handler {
	h.dests = d
	return h
}

// NewHandler creates a new admin settings handler.
func NewHandler(store repo.Store, bk *backup.Service, admin *service.AdminService, rt *runtime.Runtime, px *svcproxy.Service, gh *githubapp.Client, signer *registry.Signer, dataDir, registryPort, acmeEmail, baseURL string) *handler {
	return &handler{store: store, backups: bk, admin: admin, rt: rt, px: px, gh: gh, signer: signer, dataDir: dataDir,
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
	users, err := h.auth.Users(c.Request().Context())
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, usersPage(c, users))
}

// GET /admin/registries
func (h *handler) Registries(c echo.Context) error {
	regs, err := h.registries.ListAll(c.Request().Context())
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, registriesPage(c, regs))
}

// GET /admin/tls
func (h *handler) TLS(c echo.Context) error {
	ctx := c.Request().Context()
	dnsProvider, _ := h.settings.Value(ctx, "dns_provider")
	dnsEnv, _ := h.settings.Value(ctx, "dns_env")
	// Absent is fine here: the page renders the "not configured" state.
	managed, err := h.registries.Managed(ctx)
	if err != nil && !errors.Is(err, svcerr.ErrUnavailable) {
		return err
	}
	return respond.HTML(c, http.StatusOK, tlsPage(c, dnsProvider, dnsEnv, h.acmeEmail, managed))
}

// GET /admin/maintenance
func (h *handler) Maintenance(c echo.Context) error {
	cleanup, _ := h.settings.Value(c.Request().Context(), "cleanup_enabled")
	interval := strconv.Itoa(h.watch.Interval(c.Request().Context()))
	return respond.HTML(c, http.StatusOK, maintenancePage(c, cleanup == "1", interval))
}

// POST /admin/imagewatch, set the registry check cadence (minutes, 0 = off).
func (h *handler) SaveImageWatch(c echo.Context) error {
	if err := h.watch.SetInterval(c.Request().Context(), c.FormValue("interval_minutes")); err != nil {
		if !stackrmw.FlashRefusal(c, err) {
			return err
		}
		return respond.Redirect(c, "/admin/maintenance")
	}
	n := h.watch.Interval(c.Request().Context())
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
	o, err := h.orgs.Resolve(c.Request().Context(), orgID)
	if err != nil {
		return "", stackrmw.HTTP(err)
	}
	orgID = o.ID
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
//
// Kept as a function because other packages call it; the rule itself is
// BackupDestinationService's.
func ListDestinations(ctx context.Context, store repo.Store, orgID string) ([]repo.BackupDestination, error) {
	return service.NewBackupDestinationService(store, nil).AtScope(ctx, orgID)
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
	all, err := h.schedules.ListAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range all {
		if all[i].Kind == repo.BackupStackr {
			runs, _ := h.schedules.Runs(ctx, all[i].ID, 10)
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
	dest, err := h.dests.Get(ctx, c.FormValue("destination_id"))
	if err != nil && !errors.Is(err, svcerr.ErrNotFound) {
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
	h.sched.ReloadBackups(ctx)
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
	if _, err := h.backups.Start(ctx, b.ID, "manual"); err != nil {
		middleware.SetFlash(c, "Backup failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Backup started.", middleware.FlashSuccess)
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
	h.sched.ReloadBackups(ctx)
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
	// Server-wide: shared decides whether every org may write into it. Off
	// unless asked, because the bucket credentials go with it.
	if _, err := h.dests.Create(c.Request().Context(), service.NewDestination{
		Name:      c.FormValue("name"),
		Endpoint:  c.FormValue("endpoint"),
		Region:    c.FormValue("region"),
		Bucket:    c.FormValue("bucket"),
		AccessKey: c.FormValue("access_key"),
		SecretKey: c.FormValue("secret_key"),
		OrgID:     orgID,
		Shared:    orgID == "" && c.FormValue("shared") != "",
	}); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return h.destinationsDone(c, orgID)
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
	d, err := h.dests.Get(ctx, c.Param("destID"))
	if err != nil && !errors.Is(err, svcerr.ErrNotFound) {
		return err
	}
	// Scope check, not just existence: an org page may only delete that org's
	// destinations, and the admin page only server-wide ones.
	if d == nil || d.OrgID.String != orgID {
		return echo.NewHTTPError(http.StatusNotFound, "destination not found")
	}
	if err := h.dests.Delete(ctx, d); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Destination removed. Its schedules went with it; the archives in the bucket did not.", middleware.FlashSuccess)
	return h.destinationsDone(c, orgID)
}

func (h *handler) destinationsDone(c echo.Context, orgID string) error {
	if orgID == "" {
		return respond.Redirect(c, "/admin/backups")
	}
	o, _ := h.orgs.Get(c.Request().Context(), orgID)
	if o == nil {
		return respond.Redirect(c, "/")
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/backups")
}

// POST /admin/dns, DNS-01 provider for wildcard certs. Traefik is recreated
// in the background when provider or credentials changed.
func (h *handler) SaveDNS(c echo.Context) error {
	ctx := c.Request().Context()
	if err := h.px.SetDNS(ctx, c.FormValue("dns_provider"), c.FormValue("dns_env")); err != nil {
		return err
	}
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
	o, err := h.orgs.Resolve(c.Request().Context(), orgID)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	orgID = o.ID
	// Was unguarded: the org id came straight off the form, so anyone could
	// create a connector inside an org they are not a member of. The delete
	// path was hardened for exactly this; the create path was missed.
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
	org, _ := h.orgs.Get(ctx, cn.OrgID)
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
	// Refused while something still points at it. The row used to go and the
	// dangling id stayed on the tiles and stacks that named it, failing later
	// and somewhere else — in the CI gate, or in a plan that could not read
	// its own repository.
	if users, uerr := connectorUsers(ctx, h.store, cn); uerr == nil && len(users) > 0 {
		middleware.SetFlash(c, "Still used by "+strings.Join(users, ", ")+". Point those at another connector first.", middleware.FlashError)
		org, _ := h.orgs.Get(ctx, cn.OrgID)
		if org == nil {
			return respond.Redirect(c, "/")
		}
		return respond.Redirect(c, "/orgs/"+org.Slug+"/settings/connectors")
	}
	if err := h.store.DeleteConnector(ctx, cn.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Connector removed. Delete the GitHub App itself at github.com/settings/apps if you no longer need it.", middleware.FlashSuccess)
	org, _ := h.orgs.Get(ctx, cn.OrgID)
	if org == nil {
		return respond.Redirect(c, "/")
	}
	return respond.Redirect(c, "/orgs/"+org.Slug+"/settings/connectors")
}

// connectorUsers names what would be left holding a dead connector id: the
// org's own config binding, any stack bound through it, and any tile built
// from it.
func connectorUsers(ctx context.Context, store repo.Store, cn *repo.Connector) ([]string, error) {
	var out []string
	if org, err := store.GetOrg(ctx, cn.OrgID); err == nil && org != nil && org.ConfigConnectorID == cn.ID {
		out = append(out, "the "+org.Name+" organization's config binding")
	}
	stacks, err := store.ListStacksByOrg(ctx, cn.OrgID)
	if err != nil {
		return nil, err
	}
	for i := range stacks {
		if stacks[i].ConfigConnectorID == cn.ID {
			out = append(out, stacks[i].Slug+"'s config binding")
		}
		tiles, terr := store.ListTilesByStack(ctx, stacks[i].ID)
		if terr != nil {
			continue
		}
		for j := range tiles {
			if tiles[j].ConnectorID == cn.ID {
				out = append(out, stacks[i].Slug+"/"+tiles[j].Slug)
			}
		}
	}
	return out, nil
}

// POST /admin/users/:id/admin, grant or revoke server-admin rights.
func (h *handler) ToggleUserAdmin(c echo.Context) error {
	ctx := c.Request().Context()
	u, err := h.auth.User(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if u.Role == "admin" {
		// Demotion: never remove the last admin.
		users, err := h.auth.Users(ctx)
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
	if err := h.auth.SaveUser(ctx, u); err != nil {
		return err
	}
	middleware.SetFlash(c, "User updated.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/users")
}

// POST /admin/users/:id/toggle, enable or disable a user account.
func (h *handler) ToggleUserActive(c echo.Context) error {
	ctx := c.Request().Context()
	u, err := h.auth.User(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if u.Role == "admin" {
		middleware.SetFlash(c, "The server admin cannot be disabled.", middleware.FlashError)
		return respond.Redirect(c, "/admin/users")
	}
	u.Active = !u.Active
	u.UpdatedAt = time.Now().UTC()
	if err := h.auth.SaveUser(ctx, u); err != nil {
		return err
	}
	// Disabling only. The share links this user minted answer to whoever holds
	// the URL, with no user on the redeem path, so they outlive the account
	// unless they are closed here — see service.RevokeService.
	if !u.Active {
		if err := h.revoke.UserDeactivated(ctx, u.ID); err != nil {
			return err
		}
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
	if err := h.settings.SetValue(c.Request().Context(), "cleanup_enabled", v); err != nil {
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
	reg, err := h.registries.Managed(ctx)
	if errors.Is(err, svcerr.ErrUnavailable) {
		return echo.NewHTTPError(http.StatusBadRequest, "enable the managed registry first")
	}
	if err != nil {
		return err
	}
	if err := h.px.SetRegistryDomain(ctx, reg, c.FormValue("domain")); err != nil {
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
	if _, err := h.registries.AddExternal(c.Request().Context(), c.FormValue("name"),
		c.FormValue("url"), c.FormValue("username"), c.FormValue("password")); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Registry added.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/registries")
}

// POST /admin/registries/:id/delete
func (h *handler) DeleteRegistry(c echo.Context) error {
	// Through the service, which refuses the managed row. This handler did
	// not load the registry at all, so one POST removed it; boot then
	// recreated it with a fresh password and every org's derived credential
	// stopped working until the next EnsureSystemCredential.
	if err := h.registries.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return stackrmw.HTTP(err)
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
	d, err := h.dests.Get(ctx, c.Param("destID"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if !d.Global() {
		return echo.NewHTTPError(http.StatusBadRequest, "sharing applies to server-wide destinations only")
	}
	want := c.FormValue("shared") != ""
	if err := h.dests.SetShared(ctx, d, want); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return respond.Redirect(c, "/admin/backups")
	}
	if want {
		middleware.SetFlash(c, "Shared with every organization.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "No longer shared.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/admin/backups")
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
	events, err := h.audit.All(ctx, actor, action, auditPageSize)
	if err != nil {
		return err
	}
	// The actor list comes from the unfiltered page, so picking one does not
	// remove every other name from the dropdown.
	all, err := h.audit.All(ctx, "", "", auditPageSize)
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

// WithImageWatch gives the handler the registry watch.
func (h *handler) WithImageWatch(w *service.ImageWatchService) *handler { h.watch = w; return h }

// WithScheduler gives the handler the schedule reloader.
func (h *handler) WithScheduler(s *scheduler.Service) *handler { h.sched = s; return h }

// WithOrgs gives the page the organization service.
func (h *handler) WithOrgs(v *service.OrgService) *handler { h.orgs = v; return h }

// WithSettings gives the page the settings service.
func (h *handler) WithSettings(v *service.SettingsService) *handler { h.settings = v; return h }

// WithSchedules gives the page the backup-schedule service.
func (h *handler) WithSchedules(v *service.BackupScheduleService) *handler { h.schedules = v; return h }

// WithAuth gives the page the account service.
func (h *handler) WithAuth(v *service.AuthService) *handler { h.auth = v; return h }

// WithAudit gives the page the audit trail.
func (h *handler) WithAudit(v *service.AuditService) *handler { h.audit = v; return h }
