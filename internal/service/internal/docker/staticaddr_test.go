package docker

import (
	"net/netip"
	"testing"
)

// Docker's own picks stay in the upper half; pinned VIPs come from the lower
// half, stable per key and clear of taken addresses.
func TestStaticAddr(t *testing.T) {
	sub := netip.MustParsePrefix("10.208.3.0/24")
	if got := DynamicRange(sub).String(); got != "10.208.3.128/25" {
		t.Fatalf("DynamicRange = %s", got)
	}
	none := func(netip.Addr) bool { return false }
	a := StaticAddr(sub, "tile-1", none)
	if a != StaticAddr(sub, "tile-1", none) {
		t.Fatal("not stable for the same key")
	}
	ip := netip.MustParseAddr(a)
	if !sub.Contains(ip) || ip.As4()[3] < 2 || ip.As4()[3] > 127 {
		t.Fatalf("%s is not in the lower half past the gateway", a)
	}
	b := StaticAddr(sub, "tile-1", func(x netip.Addr) bool { return x == ip })
	if b == a || b == "" {
		t.Fatalf("taken %s, got %s", a, b)
	}
	if got := StaticAddr(sub, "k", func(netip.Addr) bool { return true }); got != "" {
		t.Fatalf("full half gave %s", got)
	}
}
