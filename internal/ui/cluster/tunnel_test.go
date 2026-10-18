package cluster_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/ui/cluster"
)

// The tunnel drawer names both ends, who dials and what each reports.
func TestTunnelBody(t *testing.T) {
	m := sample()
	hs := time.Now().Add(-2 * time.Minute)
	l := cluster.Link{A: "h", B: "f", Kind: "internet", MS: 21, Ends: [2]cluster.End{
		{Dials: true, Endpoint: "203.0.113.5:51820", SeenFrom: "203.0.113.5:51820", Handshake: hs, Rx: 2 << 20, Tx: 512},
		{Rx: -1, Tx: -1},
	}}
	var sb strings.Builder
	if err := cluster.TunnelBody(m, l, "1h").Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"home", "fsn1", `hx-get="/-/admin/nodes/f"`, "Internet", "21 ms", "203.0.113.5:51820",
		"2 min ago", "2.0 MB", "0.5 KB", "waits to be dialled", "not reported", "Round trip", "Packet loss", `unit="ratio"`} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("tunnel drawer lacks %q", want)
		}
	}
	if cluster.TunnelNames(m, l) != "home and fsn1" {
		t.Errorf("title = %q", cluster.TunnelNames(m, l))
	}
}
