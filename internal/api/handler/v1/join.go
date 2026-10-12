package v1

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// The two unauthenticated join calls share one budget per address: the token
// is the only credential, so guessing it must stay hopeless.
const (
	joinPerWindow = 10
	joinWindow    = time.Minute
	joinMaxKeys   = 10000
)

var joinTries = struct {
	sync.Mutex
	m map[string]struct {
		n     int
		reset time.Time
	}
}{m: map[string]struct {
	n     int
	reset time.Time
}{}}

// joinAllowed counts one try for ip and says whether it may go on.
func joinAllowed(ip string) (bool, time.Duration) {
	joinTries.Lock()
	defer joinTries.Unlock()
	now := time.Now()
	if len(joinTries.m) >= joinMaxKeys {
		for k, w := range joinTries.m {
			if !now.Before(w.reset) {
				delete(joinTries.m, k)
			}
		}
	}
	w, ok := joinTries.m[ip]
	if !ok && len(joinTries.m) >= joinMaxKeys {
		return false, joinWindow
	}
	if !now.Before(w.reset) {
		w.n, w.reset = 0, now.Add(joinWindow)
	}
	if w.n >= joinPerWindow {
		return false, time.Until(w.reset)
	}
	w.n++
	joinTries.m[ip] = w
	return true, 0
}

// joinLimited wraps a public join endpoint in the per-address limit.
func joinLimited(e Endpoint) Endpoint {
	next := e.Handle
	e.Handle = func(c echo.Context) error {
		if ok, wait := joinAllowed(c.RealIP()); !ok {
			c.Response().Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			return echo.NewHTTPError(http.StatusTooManyRequests, "too many join attempts; try again later")
		}
		return next(c)
	}
	return e.Limit(64 << 10)
}

// CreateJoinToken (server admin) mints a single use token for a new worker
// and answers the one line to run on that machine.
func (h *H) CreateJoinToken() Endpoint {
	return JSON(http.StatusCreated, func(c echo.Context, in service.JoinTokenSpec) (service.JoinTokenOut, error) {
		return h.Orch.CreateJoinToken(rc(c), middleware.Principal(c), in)
	})
}

// RequestJoin (public, https only, rate limited) is the new machine posting
// its token, CSR, WireGuard key and facts. It answers pending; the machine
// then polls JoinStatus.
func (h *H) RequestJoin() Endpoint {
	return joinLimited(JSON(http.StatusAccepted, func(c echo.Context, in service.JoinRequest) (service.JoinStatus, error) {
		r := c.Request()
		return h.Orch.RequestJoin(rc(c), service.JoinMeta{
			IP: c.RealIP(), UserAgent: r.UserAgent(), HTTPS: c.Scheme() == "https",
		}, in)
	}))
}

// JoinStatusIn is the token whose request is being polled.
type JoinStatusIn struct {
	Token string `json:"token"`
}

// JoinStatus (public, rate limited) is where a join request stands; once
// confirmed it carries the node's cert and the CA cert.
func (h *H) JoinStatus() Endpoint {
	return joinLimited(JSON(http.StatusOK, func(c echo.Context, in JoinStatusIn) (service.JoinStatus, error) {
		return h.Orch.JoinStatus(rc(c), in.Token)
	}))
}

// PendingJoins (server admin) lists the nodes waiting to be confirmed.
func (h *H) PendingJoins() Endpoint {
	return Get(func(c echo.Context) ([]service.PendingJoin, error) {
		return list(h.Orch.PendingJoins(rc(c)))
	})
}

// ConfirmJoin (server admin) allocates the node's range and issues its cert.
func (h *H) ConfirmJoin() Endpoint {
	return JSON(http.StatusOK, func(c echo.Context, _ None) (service.JoinedNode, error) {
		return h.Orch.ConfirmJoin(rc(c), middleware.Principal(c), c.Param("node"))
	})
}

// RejectJoin (server admin) turns a pending node away.
func (h *H) RejectJoin() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RejectJoin(rc(c), middleware.Principal(c), c.Param("node"))
	})
}
