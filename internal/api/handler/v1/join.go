package v1

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The unauthenticated join calls (join, status) share one budget per address:
// the token is the only credential, so guessing it must stay hopeless. Recover
// and renew are signed with a node key and get a budget of their own, so
// filling the join one never blocks a worker's recovery.
const (
	joinPerWindow = 10
	nodePerWindow = 60 // workers behind one NAT share an address
	joinWindow    = time.Minute
	joinMaxKeys   = 10000
)

type window struct {
	n     int
	reset time.Time
}

// tryLimiter counts calls per client key in fixed windows.
type tryLimiter struct {
	mu        sync.Mutex
	perWindow int
	m         map[string]window
}

func newTryLimiter(perWindow int) *tryLimiter {
	return &tryLimiter{perWindow: perWindow, m: map[string]window{}}
}

var (
	joinTries = newTryLimiter(joinPerWindow)
	nodeTries = newTryLimiter(nodePerWindow)
)

// limiterKey is the address a budget is counted on: an IPv6 client is
// counted by its /64, since one host owns a whole /64 and could otherwise
// spread its calls over addresses without end.
func limiterKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Is4() || a.Is4In6() {
		return ip
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

// allow counts one try for ip and says whether it may go on.
func (l *tryLimiter) allow(ip string) (bool, time.Duration) {
	ip = limiterKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.m) >= joinMaxKeys {
		for k, w := range l.m {
			if !now.Before(w.reset) {
				delete(l.m, k)
			}
		}
	}
	w, ok := l.m[ip]
	if !ok && len(l.m) >= joinMaxKeys {
		return false, joinWindow
	}
	if !now.Before(w.reset) {
		w.n, w.reset = 0, now.Add(joinWindow)
	}
	if w.n >= l.perWindow {
		return false, time.Until(w.reset)
	}
	w.n++
	l.m[ip] = w
	return true, 0
}

// limited wraps a public endpoint in l's per-address limit.
func limited(l *tryLimiter, e Endpoint) Endpoint {
	next := e.Handle
	e.Handle = func(c echo.Context) error {
		if ok, wait := l.allow(c.RealIP()); !ok {
			c.Response().Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			return echo.NewHTTPError(http.StatusTooManyRequests, "too many join attempts; try again later")
		}
		return next(c)
	}
	return e.Limit(64 << 10)
}

func joinLimited(e Endpoint) Endpoint { return limited(joinTries, e) }
func nodeLimited(e Endpoint) Endpoint { return limited(nodeTries, e) }

// isHTTPS is whether the call came over TLS. The panel sits behind Caddy,
// which ends TLS and says so in X-Forwarded-Proto; that header counts only
// when the direct peer is one of TRUSTED_PROXIES. echo's RealIP is the peer
// itself unless the peer is trusted (then it is the forwarded client), so a
// difference means a trusted proxy sent this.
func isHTTPS(c echo.Context) bool {
	r := c.Request()
	if r.TLS != nil {
		return true
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || peer == c.RealIP() {
		return false
	}
	return strings.EqualFold(r.Header.Get(echo.HeaderXForwardedProto), "https")
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
			IP: c.RealIP(), UserAgent: r.UserAgent(), HTTPS: isHTTPS(c),
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

// RecoverNode (public, rate limited apart from join) is a worker whose controller
// went quiet fetching its mesh spec; the request is signed with its node key.
func (h *H) RecoverNode() Endpoint {
	return nodeLimited(JSON(http.StatusOK, func(c echo.Context, in worker.RecoverRequest) (worker.RecoverAnswer, error) {
		return h.Orch.RecoverNode(rc(c), in)
	}))
}

// RenewNodeCert (public, rate limited) is a worker renewing its node cert
// through the panel, signed with its current key.
func (h *H) RenewNodeCert() Endpoint {
	return nodeLimited(JSON(http.StatusOK, func(c echo.Context, in worker.RecoverRequest) (worker.RenewAnswer, error) {
		return h.Orch.RenewNodeCert(rc(c), in)
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
