package agent

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The agent is root on its node by any other name, and it is meant to listen
// on that node's stkr overlay address only. An empty result used to reach
// net.JoinHostPort("", port), which is ":port": every interface in the task's
// netns. Empty has to mean "do not listen", and the only way to be sure which
// interface is the overlay is to be told its subnet.
func TestOverlayAddrNeedsTheSubnetAndFailsClosed(t *testing.T) {
	assert.Empty(t, overlayAddr(""), "no CIDR must not resolve to an address")
	assert.Empty(t, overlayAddr("not a cidr"))
	// A range nothing local is in. 192.0.2.0/24 is reserved for documentation
	// and is never a real interface.
	assert.Empty(t, overlayAddr("192.0.2.0/24"), "matched an address outside the subnet")

	// And it does find an address when the prefix really does contain one.
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		assert.Equal(t, ipn.IP.String(), overlayAddr(ipn.String()),
			"did not find the address inside its own subnet")
		return
	}
	t.Skip("no non-loopback IPv4 interface to match against")
}
