package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/route"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// certGrace is how long a new host may go without a certificate: the proxy
// asks for it when the host is added and needs a moment.
const certGrace = 3 * time.Hour

// certProbe reads the leaf certificate addr serves for host on :443. Tests
// swap it.
var certProbe = func(ctx context.Context, addr, host string) (*x509.Certificate, error) {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		// The chain is not judged here, only whether the node has a live
		// certificate for the name; a private CA's host is fine.
		Config: &tls.Config{ServerName: host, InsecureSkipVerify: true}, //nolint:gosec
	}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr, "443"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("no certificate sent")
	}
	return certs[0], nil
}

// certHost is a host an ingress node serves with HTTPS; Added is when it
// was set up (zero = long ago).
type certHost struct {
	Host  string
	Added time.Time
}

// certHosts are the hosts each ingress node serves with HTTPS, by node id:
// the panel domain, external routes that are not pass-through and tile
// domains with HTTPS on. Wildcards are skipped (no name to dial), so are
// bare names and addresses, which have no public certificate. complete is
// false when a read failed, so a host may be missing. TLS off serves none.
func (o *Orchestrator) certHosts(ctx context.Context) (out map[string][]certHost, complete bool) {
	out = map[string][]certHost{}
	if o.cfg.TLSOff {
		return out, true
	}
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return out, false
	}
	ing, err := o.listIngressNodes(ctx)
	complete = err == nil
	add := func(nodeID, host string, added time.Time) {
		if _, err := netip.ParseAddr(host); err != nil && strings.Contains(host, ".") && !strings.HasPrefix(host, "*.") {
			out[nodeID] = append(out[nodeID], certHost{host, added})
		}
	}
	p, err := o.settings.Get(ctx, "panel_domain")
	complete = complete && err == nil
	if p != "" {
		add(def, p, time.Time{})
	}
	rs, err := o.routes.List(ctx)
	complete = complete && err == nil
	for _, r := range rs {
		if r.Mode != route.Passthrough {
			add(def, r.Host, r.CreatedAt)
		}
	}
	ds, err := o.store.Domains.List(ctx)
	complete = complete && err == nil
	for _, d := range ds {
		if !d.HTTPS {
			continue
		}
		at, err := o.IngressFor(ctx, d.TileID)
		if err != nil {
			complete = false
			continue
		}
		if at == "" {
			at = def
		}
		if at == def || slices.Contains(ing, at) {
			add(at, d.Host, d.CreatedAt)
		}
	}
	return out, complete
}

// certAddr is where to dial a node's proxy: the controller (and a one-box
// install) by loopback, a worker by the public address its DNS points at
// (IPv4 first): the worker firewall drops new TCP on the mesh interface
// except from the controller on its API port. "" = no public address.
func (o *Orchestrator) certAddr(ctx context.Context, n store.Node) string {
	if o.cluster == nil || n.Role == node.Controller {
		return "127.0.0.1"
	}
	v4, v6, _, err := o.nodeAddrs(ctx, n.ID)
	switch {
	case err != nil:
		return ""
	case v4.IsValid():
		return v4.String()
	case v6.IsValid():
		return v6.String()
	}
	return ""
}

// certProblem says what is wrong with the certificate a node serves for
// host, "" when nothing is. A host added within certGrace is given time.
func certProblem(c *x509.Certificate, err error, host string, added, now time.Time) string {
	young := now.Sub(added) < certGrace
	switch {
	case err != nil:
		if young {
			return ""
		}
		return "no certificate (" + oneLine(err.Error()) + ")"
	case c.VerifyHostname(host) != nil:
		if young {
			return ""
		}
		return "the certificate is for another name"
	case now.After(c.NotAfter):
		return "the certificate expired on " + c.NotAfter.UTC().Format(time.DateOnly)
	case now.Before(c.NotBefore):
		return "the certificate is not valid yet"
	case c.NotAfter.Sub(now) < certWarnDays*24*time.Hour:
		return "the certificate expires on " + c.NotAfter.UTC().Format(time.DateOnly)
	}
	return ""
}

// hostCertAlerts is the daily check: every HTTPS host of every ingress node
// is dialled with its name as SNI. A host fires cert.failing once and
// cert.ok when it is right again; the state is the row alert.certfail.<node>.<host>.
// After a complete read, the rows of hosts no longer served go.
func (o *Orchestrator) hostCertAlerts(ctx context.Context) {
	now := time.Now()
	inv, complete := o.certHosts(ctx)
	if complete {
		o.sweep(ctx, "certfail.", func(rest string) bool {
			id, host, _ := strings.Cut(rest, ".")
			return slices.ContainsFunc(inv[id], func(h certHost) bool { return h.Host == host })
		})
	}
	for nodeID, hosts := range inv {
		n, err := o.store.Nodes.Get(ctx, nodeID)
		if err != nil {
			continue
		}
		addr := o.certAddr(ctx, n)
		if addr == "" {
			continue
		}
		for _, h := range hosts {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			c, err := certProbe(cctx, addr, h.Host)
			cancel()
			o.certHostAlert(ctx, n, h.Host, certProblem(c, err, h.Host, h.Added, now))
		}
	}
}

func (o *Orchestrator) certHostAlert(ctx context.Context, n store.Node, host, problem string) {
	key := "alert.certfail." + n.ID + "." + host
	_, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	if problem == "" {
		o.clearProblem(ctx, key)
		if was {
			_ = o.store.Settings.Delete(ctx, key)
			o.alert(Alert{Event: AlertCertOK, Severity: SevInfo, Node: n.Name, Subject: host,
				Message: fmt.Sprintf("%s has a good certificate on %s again.", host, n.Name)})
		}
		return
	}
	msg := fmt.Sprintf("%s on %s: %s.", host, n.Name, problem)
	o.openProblem(ctx, key, SevCritical, msg)
	if !was && o.store.Settings.Set(ctx, key, "1") == nil {
		o.alert(Alert{Event: AlertCertFailing, Severity: SevCritical, Node: n.Name, Subject: host, Message: msg})
	}
}
