// Package cli implements the browser side of the `stackr` CLI login flow.
//
// gh-style loopback code exchange: the CLI opens the browser at
// GET /cli/authorize (behind the web session), the user approves, and the
// server mints an API key but hands the browser only a one-time *code*. The
// CLI then exchanges that code for the raw key over a direct request to
// POST /cli/exchange. The raw key never touches the browser (history sync
// would otherwise carry it off the machine).
package cli

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	v1 "github.com/FyrmForge/stackr/internal/stackrd/handlers/api/v1"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/account"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const codeTTL = 2 * time.Minute

// pending is an approved-but-not-yet-collected grant. The API key is minted
// only when the CLI exchanges the code, so an abandoned or timed-out login
// leaves nothing in the database, this record just expires in memory.
type pending struct {
	userID string
	// orgID binds the minted key to the org that justified its scopes, empty
	// for a server admin and for no active org. Captured here, at the approve
	// step: the exchange is unauthenticated and has no session to read it off.
	orgID    string
	scopes   string // JSON array
	hostname string
	expiry   time.Time
}

type handler struct {
	store repo.Store
	// keys owns the token format and the key row.
	keys  *service.APIKeyService
	mu    sync.Mutex
	codes map[string]pending
}

// NewHandler creates the CLI-login handler. Codes live in memory only; a
// pending login that outlives a restart just fails and the user retries.
func NewHandler(store repo.Store) *handler {
	return &handler{store: store, keys: service.NewAPIKeyService(store), codes: map[string]pending{}}
}

// grantable returns the scopes this user may put on a key: read scopes always,
// write scopes only with content-write (matches settings key creation).
func grantable(c echo.Context) []v1.ScopeInfo {
	canWrite := stackrmw.CanWrite(c) || stackrmw.IsAdmin(c)
	out := make([]v1.ScopeInfo, 0, len(v1.Scopes))
	for _, s := range v1.Scopes {
		if s.Write && !canWrite {
			continue
		}
		out = append(out, s)
	}
	return out
}

// mintOrgName names the org the key will be bound to, empty when unbound.
func mintOrgName(c echo.Context) string {
	id := account.MintOrg(c)
	if id == "" {
		return ""
	}
	if o, ok := stackrmw.ActiveOrg(c); ok && o.ID == id {
		return o.Name
	}
	return ""
}

// GET /cli/authorize?port=<n>&state=<s>&hostname=<h>, the approve page.
func (h *handler) Authorize(c echo.Context) error {
	port := c.QueryParam("port")
	if !validPort(port) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid port")
	}
	state := c.QueryParam("state")
	if state == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing state")
	}
	return respond.HTML(c, http.StatusOK, authorizePage(c, authorizeView{
		Port:     port,
		State:    state,
		Hostname: hostname(c.QueryParam("hostname")),
		Scopes:   grantable(c),
		Org:      mintOrgName(c),
	}))
}

// POST /cli/authorize, mint the key, stash it under a one-time code, and
// bounce the browser to the CLI's loopback listener with only that code.
func (h *handler) Approve(c echo.Context) error {
	port := c.FormValue("port")
	if !validPort(port) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid port")
	}
	state := c.FormValue("state")
	if state == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing state")
	}

	form, _ := c.FormParams()
	granted := v1.GrantableScopes(form["scopes"], stackrmw.CanWrite(c) || stackrmw.IsAdmin(c))
	if len(granted) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "select at least one permission")
	}
	scopesJSON, _ := json.Marshal(granted)

	// Stash the grant, not a key: the key is minted at exchange time so an
	// abandoned flow never persists an unusable key.
	code := uuid.New().String()
	h.mu.Lock()
	h.purgeLocked()
	h.codes[code] = pending{
		userID:   middleware.GetSubjectID(c),
		orgID:    account.MintOrg(c),
		scopes:   string(scopesJSON),
		hostname: hostname(c.FormValue("hostname")),
		expiry:   time.Now().Add(codeTTL),
	}
	h.mu.Unlock()

	// Loopback only: port is numeric and the host is a fixed literal, so this
	// URL can never be steered off the local machine.
	dest := "http://127.0.0.1:" + port + "/callback?code=" + code + "&state=" + state
	return respond.Redirect(c, dest)
}

type exchangeReq struct {
	Code string `json:"code"`
}

type exchangeResp struct {
	Key string `json:"key"`
}

// POST /cli/exchange, unauthenticated: the one-time code is the only
// credential (the caller has neither a key nor a session yet). Burns the code.
func (h *handler) Exchange(c echo.Context) error {
	var req exchangeReq
	if err := c.Bind(&req); err != nil || req.Code == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing code")
	}
	h.mu.Lock()
	h.purgeLocked()
	p, ok := h.codes[req.Code]
	delete(h.codes, req.Code)
	h.mu.Unlock()
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "unknown or expired code")
	}

	// The scopes were filtered and stashed at the grant step, above; this
	// path is unauthenticated, so it has nobody to filter against.
	var scopes []string
	if err := json.Unmarshal([]byte(p.scopes), &scopes); err != nil {
		return err
	}
	_, raw, err := h.keys.Mint(c.Request().Context(), p.userID, p.orgID,
		"stackr CLI ("+p.hostname+")", scopes)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, exchangeResp{Key: raw})
}

// purgeLocked drops expired codes; caller holds h.mu.
func (h *handler) purgeLocked() {
	now := time.Now()
	for code, p := range h.codes {
		if now.After(p.expiry) {
			delete(h.codes, code)
		}
	}
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// hostname trims a client-supplied hostname to something safe for a key label.
func hostname(h string) string {
	if h == "" {
		return "unknown host"
	}
	if r := []rune(h); len(r) > 64 {
		return string(r[:64]) // truncate on a rune boundary, not mid-byte
	}
	return h
}
