package metrics

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlowFields(t *testing.T) {
	line := "ipv4     2 tcp      6 431999 ESTABLISHED src=172.19.0.4 dst=172.19.0.2 sport=54321 dport=5432 packets=10 bytes=1234 src=172.19.0.2 dst=172.19.0.4 sport=5432 dport=54321 packets=8 bytes=5678 [ASSURED] mark=0 use=1"
	src, sport, dst, dport, b := flowFields(line, 0)
	require.Equal(t, "172.19.0.4", src, "orig group wrong")
	require.Equal(t, "54321", sport, "orig group wrong")
	require.Equal(t, "172.19.0.2", dst, "orig group wrong")
	require.Equal(t, "5432", dport, "orig group wrong")
	require.Equal(t, uint64(1234), b, "orig group wrong")
	_, _, _, _, rb := flowFields(line, 1)
	require.Equal(t, uint64(5678), rb, "reply bytes wrong")
	// acct disabled: no bytes fields at all
	_, _, _, _, nb := flowFields("ipv4 2 tcp 6 src=1.2.3.4 dst=5.6.7.8 sport=1 dport=2 src=5.6.7.8 dst=1.2.3.4 sport=2 dport=1", 0)
	require.Equal(t, uint64(0), nb, "want 0 bytes without acct")
}
