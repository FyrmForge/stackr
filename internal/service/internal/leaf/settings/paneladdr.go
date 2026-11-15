package settings

import (
	"net/netip"
	"strings"
)

// The panel_address modes: auto follows the server's public address,
// ip:<addr> is one the admin typed, self writes no record at all.
const (
	PanelAuto = "auto"
	PanelIP   = "ip"
	PanelSelf = "self"
)

// ParsePanelAddress reads a panel_address value: "" and "auto", "self",
// "ip:<addr>", or a bare address as shorthand for it. norm is the stored
// form: ip:<addr> with the address in its canonical spelling.
func ParsePanelAddress(raw string) (mode string, ip netip.Addr, norm string, ok bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "", PanelAuto:
		return PanelAuto, ip, raw, true
	case PanelSelf:
		return PanelSelf, ip, raw, true
	}
	ip, err := netip.ParseAddr(strings.TrimPrefix(raw, PanelIP+":"))
	if err != nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.Is4In6() || ip.Zone() != "" {
		return "", netip.Addr{}, "", false
	}
	return PanelIP, ip, PanelIP + ":" + ip.String(), true
}
