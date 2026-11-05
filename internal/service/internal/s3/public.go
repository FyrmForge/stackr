package s3

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// errNotPublic is a dial the public-only client refused: the endpoint is a
// private address no server admin approved.
var errNotPublic = errors.New("waiting for the server admin to approve LAN access")

// cgnat is the shared address space (100.64.0.0/10), not private for
// netip but never a public bucket.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicIP: a global unicast address that is not private, loopback,
// link-local (cloud metadata included) or shared space.
func publicIP(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

// Addr is the host:port an endpoint URL dials (default port by scheme); ""
// for an empty or unparsable one.
func Addr(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// resolve is the addresses of a host:port, an IP literal as itself.
func resolve(addr string) []netip.AddrPort {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	ips := []netip.Addr{}
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = append(ips, ip)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ips, _ = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	var out []netip.AddrPort
	p, _ := net.LookupPort("tcp", port)
	for _, ip := range ips {
		out = append(out, netip.AddrPortFrom(ip.Unmap(), uint16(p)))
	}
	return out
}

// Private reports whether the endpoint names a non-public address (one of
// its resolved addresses is not public).
func Private(endpoint string) bool {
	for _, ap := range resolve(Addr(endpoint)) {
		if !publicIP(ap.Addr()) {
			return true
		}
	}
	return false
}

// publicClient dials only public addresses, checked on the address actually
// connected to (so DNS rebinding and redirects cannot reach an internal host),
// plus the addresses of allow (an approved host:port, "" = none).
// A member's endpoint is theirs to name; the panel's network is not.
func publicClient(allow string) *http.Client {
	ok := resolve(allow)
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return errNotPublic
			}
			ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
			for _, a := range ok {
				if a == ap {
					return nil
				}
			}
			if !publicIP(ap.Addr()) {
				return errNotPublic
			}
			return nil
		},
	}
	return &http.Client{Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second}}
}
