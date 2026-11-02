package s3

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// errNotPublic is a dial the public-only client refused.
var errNotPublic = errors.New("endpoint is not a public address")

// cgnat is the shared address space (100.64.0.0/10), not private for
// netip but never a public bucket.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicIP: a global unicast address that is not private, loopback,
// link-local (cloud metadata included) or shared space.
func publicIP(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

// publicClient dials only public addresses, checked on the address actually
// connected to (so DNS rebinding and redirects cannot reach an internal host).
// A member's endpoint is theirs to name; the panel's network is not.
func publicClient() *http.Client {
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !publicIP(ap.Addr()) {
				return errNotPublic
			}
			return nil
		},
	}
	return &http.Client{Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second}}
}
