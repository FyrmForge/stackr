// Package account owns everything scoped to the signed-in person: their
// profile, their password, their API keys and their notification preferences.
//
// These used to be scattered, API keys sat on the global settings page beside
// instance-wide infrastructure config, and there was no way at all to change a
// password. Splitting them out is what lets the settings gear become
// admin-only without stranding ordinary users.
package account

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/storage"

	v1 "github.com/FyrmForge/stackr/internal/stackrd/handlers/api/v1"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/avatar"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store repo.Store
	auth  *service.AuthService
	files storage.FileStorage
	// keys owns the token format and the key row, which this page, the CLI
	// login's grant step and its exchange each wrote out for themselves.
	keys *service.APIKeyService
}

// NewHandler creates a new account handler.
func NewHandler(store repo.Store, auth *service.AuthService, files storage.FileStorage) *handler {
	return &handler{store: store, auth: auth, files: files, keys: service.NewAPIKeyService(store)}
}

// me loads the signed-in user.
func (h *handler) me(c echo.Context) (*repo.User, error) {
	u, err := h.auth.User(c.Request().Context(), middleware.GetSubjectID(c))
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	return u, nil
}

// GET /account, the profile section is the landing.
func (h *handler) Index(c echo.Context) error {
	return respond.Redirect(c, "/account/profile")
}

// GET /account/profile
func (h *handler) Profile(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, profilePage(c, u))
}

// POST /account/profile, display name and email.
func (h *handler) SaveProfile(c echo.Context) error {
	ctx := c.Request().Context()
	u, err := h.me(c)
	if err != nil {
		return err
	}
	email := strings.TrimSpace(c.FormValue("email"))
	if email == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "email required")
	}
	// Email is the login identifier, so a collision would lock someone out.
	if email != u.Email {
		if existing, _ := h.auth.UserByEmail(ctx, email); existing != nil {
			middleware.SetFlash(c, "That email is already registered.", middleware.FlashError)
			return respond.Redirect(c, "/account/profile")
		}
		u.Email = email
	}
	u.Name = strings.TrimSpace(c.FormValue("name"))
	// An empty file input is the normal case (the form saves name and email
	// too), so "no file" is not an error.
	if fh, err := c.FormFile("avatar"); err == nil && fh != nil {
		p, err := avatar.Replace(ctx, h.files, "users", u.ID, u.AvatarPath, fh)
		if err != nil {
			middleware.SetFlash(c, err.Error(), middleware.FlashError)
			return respond.Redirect(c, "/account/profile")
		}
		u.AvatarPath = p
	}
	if c.FormValue("remove_avatar") == "1" && u.AvatarPath != "" {
		_ = h.files.Delete(ctx, u.AvatarPath)
		u.AvatarPath = ""
	}
	u.UpdatedAt = time.Now().UTC()
	if err := h.auth.SaveUser(ctx, u); err != nil {
		return err
	}
	middleware.SetFlash(c, "Profile saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/account/profile")
}

// POST /account/password
func (h *handler) ChangePassword(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	current := c.FormValue("current_password")
	next := c.FormValue("new_password")
	if len(next) < 8 {
		middleware.SetFlash(c, "New password must be at least 8 characters.", middleware.FlashError)
		return respond.Redirect(c, "/account/profile")
	}
	if next != c.FormValue("confirm_password") {
		middleware.SetFlash(c, "New passwords do not match.", middleware.FlashError)
		return respond.Redirect(c, "/account/profile")
	}
	if err := h.auth.ChangePassword(c.Request().Context(), u.ID, current, next); err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			middleware.SetFlash(c, "Current password is incorrect.", middleware.FlashError)
			return respond.Redirect(c, "/account/profile")
		}
		return err
	}
	middleware.SetFlash(c, "Password changed.", middleware.FlashSuccess)
	return respond.Redirect(c, "/account/profile")
}

// GET /account/apikeys
func (h *handler) APIKeys(c echo.Context) error {
	keys, err := h.myKeys(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, apiKeysPage(c, keys, h.takeNewKey(c)))
}

// newKeyCookie carries a token just minted across the redirect to this page,
// which renders it once with a copy button. It used to ride the flash, and a
// flash is built to disappear on its own: miss it and the key is dead. Same
// one-shot exposure as that was (HttpOnly, seconds, cleared on read), the
// difference is that it waits on the page instead of on a timer.
const newKeyCookie = "stackr_new_key"

func (h *handler) setNewKey(c echo.Context, raw string) {
	c.SetCookie(&http.Cookie{
		Name: newKeyCookie, Value: raw, Path: "/account/apikeys", MaxAge: 60,
		HttpOnly: true, Secure: c.Scheme() == "https", SameSite: http.SameSiteLaxMode,
	})
}

func (h *handler) takeNewKey(c echo.Context) string {
	ck, err := c.Cookie(newKeyCookie)
	if err != nil || ck.Value == "" {
		return ""
	}
	c.SetCookie(&http.Cookie{
		Name: newKeyCookie, Path: "/account/apikeys", MaxAge: -1,
		HttpOnly: true, Secure: c.Scheme() == "https", SameSite: http.SameSiteLaxMode,
	})
	return ck.Value
}

// myKeys returns the current user's API keys. Keys are user-scoped, so this
// never shows one person another's keys, not even for an admin, who has their
// own list here and the audit trail elsewhere.
func (h *handler) myKeys(c echo.Context) ([]repo.APIKey, error) {
	all, err := h.keys.ListAll(c.Request().Context())
	if err != nil {
		return nil, err
	}
	sub := middleware.GetSubjectID(c)
	mine := make([]repo.APIKey, 0, len(all))
	for _, k := range all {
		if k.UserID == sub {
			mine = append(mine, k)
		}
	}
	return mine, nil
}

// POST /account/apikeys, create a key; the token is shown once.
func (h *handler) CreateAPIKey(c echo.Context) error {
	// Write scopes require content-write in the active org (or server admin):
	// a key must never grant more than the person minting it already has.
	form, _ := c.FormParams()
	granted := v1.GrantableScopes(form["scopes"], CanGrantWrite(c))
	_, raw, err := h.keys.Mint(c.Request().Context(), middleware.GetSubjectID(c),
		MintOrg(c), c.FormValue("name"), granted)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	h.setNewKey(c, raw)
	middleware.SetFlash(c, "API key created.", middleware.FlashSuccess)
	return respond.Redirect(c, "/account/apikeys")
}

// POST /account/apikeys/:id/delete
func (h *handler) DeleteAPIKey(c echo.Context) error {
	// Only the owner (or a server admin) may revoke a key.
	if !h.ownsAPIKey(c, c.Param("id")) && !stackrmw.IsAdmin(c) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := h.keys.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	middleware.SetFlash(c, "API key revoked.", middleware.FlashSuccess)
	return respond.Redirect(c, "/account/apikeys")
}

func (h *handler) ownsAPIKey(c echo.Context, id string) bool {
	keys, err := h.keys.ListAll(c.Request().Context())
	if err != nil {
		return false
	}
	sub := middleware.GetSubjectID(c)
	for i := range keys {
		if keys[i].ID == id {
			return keys[i].UserID == sub
		}
	}
	return false
}

// CanGrantWrite reports whether the current user may mint write-scoped API
// keys. Exported so the CLI authorize page applies the identical rule.
func CanGrantWrite(c echo.Context) bool {
	return stackrmw.CanWrite(c) || stackrmw.IsAdmin(c)
}

// MintOrg is the org a new key is bound to: the active one, which is what
// CanGrantWrite just answered against.
//
// Empty for a server admin. Their write scopes come from the admin badge and
// no org justified the grant, so binding the key to whichever org their cookie
// happened to point at would narrow a credential on a premise that was never
// true. Empty too when there is no active org (onboarding), which leaves the
// key unbound — see repo.APIKey. Shared with the CLI login, which grants
// against the same rule.
func MintOrg(c echo.Context) string {
	if stackrmw.IsAdmin(c) {
		return ""
	}
	o, ok := stackrmw.ActiveOrg(c)
	if !ok {
		return ""
	}
	return o.ID
}

// keyOrgName names the org a key is bound to, for the list. Empty for an
// unbound key (every key minted before the column, and every admin's), which
// the page renders as nothing rather than as a claim about its reach.
//
// Read off the session's own orgs: a key is bound to an org its owner belonged
// to at mint time, and a name that no longer resolves is one they have since
// left, where the key is dead anyway.
func keyOrgName(c echo.Context, k repo.APIKey) string {
	if !k.OrgID.Valid {
		return ""
	}
	for _, o := range stackrmw.Orgs(c) {
		if o.ID == k.OrgID.String {
			return o.Name
		}
	}
	return "another organization"
}

// GET /account/notifications
func (h *handler) Notifications(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	prefs := map[string]bool{}
	for _, k := range notify.Kinds {
		prefs[k.Kind] = notify.EnabledForUser(u, k.Kind)
	}
	return respond.HTML(c, http.StatusOK, notificationsPage(c, prefs))
}

// POST /account/notifications, this person's choices, and nobody else's.
// There is no instance-wide default to inherit from: an untouched account
// follows the built-in defaults until it is saved once.
func (h *handler) SaveNotifications(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	prefs := map[string]bool{}
	for _, k := range notify.Kinds {
		prefs[k.Kind] = c.FormValue(k.Kind) != ""
	}
	blob, _ := json.Marshal(prefs)
	u.NotifyPrefs = string(blob)
	u.UpdatedAt = time.Now().UTC()
	if err := h.auth.SaveUser(c.Request().Context(), u); err != nil {
		return err
	}
	middleware.SetFlash(c, "Notification preferences saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/account/notifications")
}

// themeOptions are the only accepted values; anything else is rejected.
var themeOptions = []struct{ Value, Label string }{
	{"system", "Match my system"},
	{"light", "Light"},
	{"dark", "Dark"},
}

// GET /account/appearance
func (h *handler) Appearance(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	theme := u.Theme
	if theme != "light" && theme != "dark" {
		theme = "system"
	}
	return respond.HTML(c, http.StatusOK, appearancePage(c, theme))
}

// POST /account/appearance
func (h *handler) SaveAppearance(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	theme := c.FormValue("theme")
	if !slices.ContainsFunc(themeOptions, func(o struct{ Value, Label string }) bool { return o.Value == theme }) {
		return echo.NewHTTPError(http.StatusBadRequest, "unknown theme")
	}
	u.Theme = theme
	u.UpdatedAt = time.Now().UTC()
	if err := h.auth.SaveUser(c.Request().Context(), u); err != nil {
		return err
	}
	middleware.SetFlash(c, "Theme saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/account/appearance")
}

// GET /account/graph-prefs, the canvas display settings as a raw JSON blob.
// The canvas client owns the shape; the server only stores it, like
// notify_prefs.
func (h *handler) GraphPrefs(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	if u.GraphPrefs == "" {
		return c.JSONBlob(http.StatusOK, []byte(`{}`))
	}
	return c.JSONBlob(http.StatusOK, []byte(u.GraphPrefs))
}

// PUT /account/graph-prefs
func (h *handler) SaveGraphPrefs(c echo.Context) error {
	u, err := h.me(c)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, 4096))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "read body")
	}
	if !json.Valid(body) {
		return echo.NewHTTPError(http.StatusBadRequest, "prefs must be JSON")
	}
	u.GraphPrefs = string(body)
	u.UpdatedAt = time.Now().UTC()
	if err := h.auth.SaveUser(c.Request().Context(), u); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// WithAuth gives the page the account service.
func (h *handler) WithAuth(v *service.AuthService) *handler { h.auth = v; return h }

// WithKeys gives the page the API-key service.
func (h *handler) WithKeys(v *service.APIKeyService) *handler { h.keys = v; return h }
