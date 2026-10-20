package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/cluster"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// The tunnel drawer finds a pair in either order, a routed pair that draws
// no line included, and shows both ends.
func TestTunnelDrawer(t *testing.T) {
	cm := service.ClusterMap{
		Nodes: []service.ClusterNode{{ID: "a", Name: "ct109"}, {ID: "b", Name: "h4"}, {ID: "c", Name: "h3"}},
		Links: []service.ClusterLink{{A: "a", B: "b", Kind: service.LinkInternet, MS: 21}},
		Pairs: []service.ClusterLink{
			{A: "a", B: "b", Kind: service.LinkInternet, MS: 21, Ends: [2]service.TunnelEnd{
				{Dials: true, Endpoint: "203.0.113.4:51820", Handshake: time.Now().Add(-3 * time.Minute), Rx: 1 << 20, Tx: 2 << 20},
				{Rx: -1, Tx: -1},
			}},
			{A: "a", B: "c", Kind: service.LinkInternet, MS: -1, Via: "h4"},
		},
	}
	render := func(a, b string) (string, bool) {
		d, ok := tunnelDrawer(cm, a, b, components.MetricsView{Range: "1h", Reload: cluster.TunnelDrawer(a, b), Events: cluster.TunnelEvents(a, b), Pick: func(string) any { return service.Series{} }})
		if !ok {
			return "", false
		}
		var sb strings.Builder
		if err := d.Render(context.Background(), &sb); err != nil {
			t.Fatal(err)
		}
		return sb.String(), true
	}
	body, ok := render("b", "a")
	if !ok {
		t.Fatal("pair not found in reverse order")
	}
	for _, want := range []string{"ct109 and h4", "203.0.113.4:51820", "3 min ago", "1.0 MB", "2.0 MB", "waits to be dialled", "Round trip", `sse-connect="/-/cluster/tunnel/events?a=a&amp;b=b"`} {
		if !strings.Contains(body, want) {
			t.Errorf("tunnel drawer lacks %q", want)
		}
	}
	routed, ok := render("a", "c")
	if !ok || !strings.Contains(routed, "Via h4") {
		t.Errorf("routed pair: ok %v, drawer lacks the hop", ok)
	}
	if _, ok := render("a", "zzz"); ok {
		t.Error("an unknown pair has a drawer")
	}
}
