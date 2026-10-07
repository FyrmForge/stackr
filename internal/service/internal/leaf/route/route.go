// Package route owns routes: hosts that go to an address outside stackr
// (raw TLS pass-through, or a reverse proxy to another machine). Admin-made,
// server-wide. Hosts of the other tables (tile domains, domain resources)
// come in as arguments, the way leaf/domainres takes the orgs.
package route

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The modes.
const (
	Passthrough = "passthrough"
	HTTP        = "http"
	HTTPS       = "https"
)

// Spec is a route someone asks for.
type Spec struct {
	Host     string
	Mode     string
	Target   string // host[:port]; the port defaults by mode
	Insecure bool   // https only: skip the upstream's certificate check
}

type Leaf struct{ rows store.RouteStore }

func New(rows store.RouteStore) *Leaf { return &Leaf{rows: rows} }

// List is every route, by host.
func (l *Leaf) List(ctx context.Context) ([]store.Route, error) { return l.rows.List(ctx) }

func (l *Leaf) Get(ctx context.Context, id string) (store.Route, error) { return l.rows.Get(ctx, id) }

// Create adds a route. taken are the hosts other tables hold (tile domains
// and domain resources); tlsOn is false under STACKR_TLS=off, dns01 whether
// wildcard certificates can be issued.
func (l *Leaf) Create(ctx context.Context, s Spec, taken []string, tlsOn, dns01 bool) (store.Route, error) {
	r, err := prepare(s, tlsOn, dns01)
	if err != nil {
		return r, err
	}
	if slicesOverlap(r.Host, taken) {
		return r, errs.Conflictf("%s is already a tile domain or domain resource.", r.Host)
	}
	mine, err := l.rows.List(ctx)
	if err != nil {
		return r, err
	}
	for _, m := range mine {
		if overlap(r.Host, m.Host) {
			return r, errs.Conflictf("%s overlaps the route for %s.", r.Host, m.Host)
		}
	}
	return r, l.rows.Create(ctx, r)
}

// Covers reports whether a route holds host (the same name, or a wildcard
// route over it): the reverse squat check for a tile domain or resource.
func (l *Leaf) Covers(ctx context.Context, host string) (bool, error) {
	mine, err := l.rows.List(ctx)
	if err != nil {
		return false, err
	}
	return Holds(mine, host), nil
}

// Holds is Covers over rows already read: for a plan that reads them once.
func Holds(rows []store.Route, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, m := range rows {
		if overlapApex(host, m.Host) {
			return true
		}
	}
	return false
}

func (l *Leaf) Delete(ctx context.Context, id string) error { return l.rows.Delete(ctx, id) }

func prepare(s Spec, tlsOn, dns01 bool) (store.Route, error) {
	r := store.Route{ID: uuid.NewString(), Mode: s.Mode, CreatedAt: time.Now().UTC()}
	switch s.Mode {
	case Passthrough, HTTP, HTTPS:
	default:
		return r, errs.Invalidf("mode", "A route is passthrough, http or https.")
	}
	if strings.ContainsAny(s.Host, "/: ") {
		return r, errs.Invalidf("host", "Give a bare hostname: no scheme, slash or port.")
	}
	host, err := installspec.CheckRoot(s.Host)
	if err != nil {
		return r, errs.Invalidf("host", "%s", err.Error())
	}
	r.Host = host
	if strings.HasPrefix(host, "*.") && s.Mode != Passthrough && !dns01 {
		return r, errs.Invalidf("host", "A wildcard host needs a DNS provider, or the passthrough mode.")
	}
	if s.Mode == Passthrough && !tlsOn {
		return r, errs.Invalidf("mode", "Pass-through needs HTTPS on.")
	}
	if s.Insecure && s.Mode != HTTPS {
		return r, errs.Invalidf("insecure", "Skipping the certificate check only applies to an https route.")
	}
	r.Insecure = s.Insecure
	if r.Target, err = checkTarget(s.Target, s.Mode); err != nil {
		return r, err
	}
	return r, nil
}

// checkTarget is host[:port] with the mode's default port filled in.
func checkTarget(t, mode string) (string, error) {
	t = strings.TrimSpace(t)
	port := "443"
	if mode == HTTP {
		port = "80"
	}
	h, p, err := net.SplitHostPort(t)
	if err != nil {
		h = strings.Trim(t, "[]")
	} else if p != "" {
		port = p
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", errs.Invalidf("target", "%s is not a port.", port)
	}
	// ponytail: the same two-character check as leaf/domain's shared validator
	// (a leaf may not import a leaf); Caddy expands {placeholders} in the
	// target, so one that names env or a file exfiltrates it. Dedupe when the
	// validator moves somewhere both may import.
	if h == "" || strings.ContainsAny(h, "/ @?#{}") || strings.Contains(h, "://") {
		return "", errs.Invalidf("target", "Give the target as host or host:port.")
	}
	return net.JoinHostPort(h, port), nil
}

func slicesOverlap(host string, hosts []string) bool {
	for _, h := range hosts {
		if overlapApex(host, strings.ToLower(h)) {
			return true
		}
	}
	return false
}

// Overlap: the same host, or one is a wildcard over the other (a wildcard
// pass-through would swallow a tile's host, whatever the table).
func Overlap(a, b string) bool { return overlap(a, b) }

func overlap(a, b string) bool {
	return a == b || covers(a, b) || covers(b, a)
}

// overlapApex is overlap plus a wildcard over the apex one label above it:
// against a domain resource, which names tiles under its own host, a
// *.<resource> route takes every generated name there. Used where the
// other side is a tile domain or resource, never between two routes.
func overlapApex(a, b string) bool {
	return overlap(a, b) || apexOf(a, b) || apexOf(b, a)
}

func apexOf(wild, host string) bool {
	rest, ok := strings.CutPrefix(wild, "*.")
	return ok && rest == host
}

// covers: a wildcard matches one label, as Caddy's host matcher and layer4's
// SNI matcher do.
func covers(wild, host string) bool {
	rest, ok := strings.CutPrefix(wild, "*.")
	if !ok {
		return false
	}
	label, ok := strings.CutSuffix(host, "."+rest)
	return ok && label != "" && !strings.Contains(label, ".")
}
