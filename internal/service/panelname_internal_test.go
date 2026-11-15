package service

import (
	"net/netip"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
)

// panel_address reads auto, self, ip:<addr> and a bare address; anything
// else is refused.
func TestParsePanelAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, mode, norm string }{
		{"", settings.PanelAuto, ""},
		{" AUTO ", settings.PanelAuto, "auto"},
		{"self", settings.PanelSelf, "self"},
		{"203.0.113.5", settings.PanelIP, "ip:203.0.113.5"},
		{"ip:2001:DB8::1", settings.PanelIP, "ip:2001:db8::1"},
	} {
		mode, _, norm, ok := settings.ParsePanelAddress(tc.in)
		if !ok || mode != tc.mode || norm != tc.norm {
			t.Errorf("%q = %q %q %v, want %q %q", tc.in, mode, norm, ok, tc.mode, tc.norm)
		}
	}
	for _, bad := range []string{"nope", "ip:", "0.0.0.0", "127.0.0.1", "stackr.example.com"} {
		if _, _, _, ok := settings.ParsePanelAddress(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// The outside node is an online worker with a public address in another
// network than the panel's node.
func TestOutsideNode(t *testing.T) {
	t.Parallel()
	m := ClusterMap{Nodes: []ClusterNode{
		{ID: "ctl", Role: "controller", Net: "home"},
		{ID: "lan", Role: "worker", Status: NodeOnline, Net: "home", Public: []string{"198.51.100.2"}},
		{ID: "off", Role: "worker", Status: "offline", Net: "far", Public: []string{"198.51.100.3"}},
		{ID: "nopub", Role: "worker", Status: NodeOnline, Net: "far"},
		{ID: "w1", Role: "worker", Status: NodeOnline, Net: "far", Public: []string{"198.51.100.4"}},
	}}
	if got := outsideNode(m, "ctl"); got != "w1" {
		t.Errorf("outside = %q, want w1", got)
	}
	if got := outsideNode(m, "nope"); got != "" {
		t.Errorf("unknown node = %q", got)
	}
}

// A resolved name matches the target when one address is the target's and
// none of a family the target has is another.
func TestPanelResolves(t *testing.T) {
	t.Parallel()
	t4 := panelTarget{V4: netip.MustParseAddr("198.51.100.1")}
	for _, tc := range []struct {
		ips  []string
		want bool
	}{
		{[]string{"198.51.100.1"}, true},
		{[]string{"198.51.100.1", "2001:db8::1"}, false}, // a stale AAAA elsewhere sends IPv6 clients astray
		{[]string{"198.51.100.9"}, false},
		{[]string{"198.51.100.1", "198.51.100.9"}, false},
		{nil, false},
	} {
		if got := panelResolves(tc.ips, t4); got != tc.want {
			t.Errorf("%v = %v, want %v", tc.ips, got, tc.want)
		}
	}
}
