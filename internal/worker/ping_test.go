package worker

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/FyrmForge/stackr/internal/mesh"
)

func TestParsePing(t *testing.T) {
	cases := []struct {
		name, out string
		want      PeerPing
		ok        bool
	}{
		{"iputils", "3 packets transmitted, 3 received, 0% packet loss, time 402ms\nrtt min/avg/max/mdev = 0.412/0.530/0.611/0.081 ms\n", PeerPing{RTTms: 0.53}, true},
		{"busybox", "3 packets transmitted, 2 packets received, 33% packet loss\nround-trip min/avg/max = 21.1/22.4/23.0 ms\n", PeerPing{RTTms: 22.4, Loss: 0.33}, true},
		{"dead", "3 packets transmitted, 0 received, 100% packet loss, time 2050ms\n", PeerPing{Loss: 1}, true},
		{"nothing", "ping: connect: Network is unreachable\n", PeerPing{}, false},
	}
	for _, c := range cases {
		got, ok := parsePing(c.out)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

type pingRunner struct{}

func (pingRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	if name != "ping" {
		return nil, nil
	}
	switch args[len(args)-1] {
	case "10.207.255.2":
		return []byte("3 packets transmitted, 3 received, 0% packet loss\nrtt min/avg/max/mdev = 1/2.5/3/0 ms\n"), nil
	default:
		return []byte("3 packets transmitted, 0 received, 100% packet loss\n"), errors.New("exit status 1")
	}
}

// Every transit /32 in the applied mesh is pinged, routed ones included (they
// sit on the next hop's entry); env networks are not.
func TestPingPeers(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), Runner: pingRunner{}})
	t.Cleanup(s.Stop)
	if got := s.pingPeers(context.Background()); got != nil {
		t.Fatalf("no mesh yet: %v", got)
	}
	s.mesh.applied = &MeshSpec{Peers: []mesh.Peer{{NodeID: "a", AllowedIPs: []netip.Prefix{
		netip.MustParsePrefix("10.208.4.0/24"),
		netip.MustParsePrefix("10.207.255.2/32"),
		netip.MustParsePrefix("10.207.255.4/32"),
	}}}}
	got := s.pingPeers(context.Background())
	if len(got) != 2 || got["10.207.255.2"] != (PeerPing{RTTms: 2.5}) || got["10.207.255.4"] != (PeerPing{Loss: 1}) {
		t.Fatalf("pings = %+v", got)
	}
}
