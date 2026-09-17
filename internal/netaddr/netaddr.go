// Package netaddr checks addresses typed by an operator. It imports nothing
// from the panel so the installer binary can share it.
package netaddr

import (
	"fmt"
	"net"
	"strings"
)

// ParseTrusted normalises one trusted proxy entry: a bare address becomes a
// single-host range (/32, /128), a range is written in its canonical form. A
// /0 is refused, it would trust every address on the internet to name the
// client.
func ParseTrusted(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		ip := net.ParseIP(s)
		if ip == nil {
			return "", fmt.Errorf("%q is not an IP address or range", s)
		}
		if ip.To4() != nil {
			return ip.String() + "/32", nil
		}
		return ip.String() + "/128", nil
	}
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return "", fmt.Errorf("%q is not an IP address or range", s)
	}
	if ones, _ := n.Mask.Size(); ones == 0 {
		return "", fmt.Errorf("%q trusts every address; name your proxy's address instead", s)
	}
	return n.String(), nil
}
