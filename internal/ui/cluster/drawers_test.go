package cluster_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/cluster"
)

func TestNoPopover(t *testing.T) {
	if strings.Contains(render(t), "inet-panel") || strings.Contains(render(t), `popovertarget="inet-`) {
		t.Error("the map still carries an internet popover")
	}
}

// Internet is the card's facts, for the map and its drawer alike.
func TestInternet(t *testing.T) {
	m := sample()
	nat, ok := cluster.Internet(m, "nat:192.168.1.0/24")
	if !ok || !nat.Behind || nat.Forward != "home" || !nat.In || len(nat.Ingress) != 3 {
		t.Errorf("nat = %+v ok %v", nat, ok)
	}
	if nat.Name != "NAT" || len(nat.EgressOwn) != 1 {
		t.Errorf("nat name %q egress %v", nat.Name, nat.EgressOwn)
	}
	f, ok := cluster.Internet(m, "node:f")
	if !ok || f.Name != "fsn1" || !f.In || len(f.EgressOwn) != 1 || len(f.EgressOthers) != 1 || len(f.Public) != 1 {
		t.Errorf("fsn1 = %+v ok %v", f, ok)
	}
	// A node behind the router has no card of its own; neither does a made-up id.
	for _, id := range []string{"node:n", "node:nope", "net:x", "nat:10.20.0.0/24"} {
		if _, ok := cluster.Internet(m, id); ok {
			t.Errorf("%s has an internet card", id)
		}
	}
	body := func(i cluster.Inet) string {
		var sb strings.Builder
		if err := cluster.InternetBody(i).Render(context.Background(), &sb); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	for _, want := range []string{"203.0.113.5", "shop.example.com", `href="/acme/prod/web"`, `href="/x/y/z"`} {
		if !strings.Contains(body(f), want) {
			t.Errorf("internet drawer lacks %q", want)
		}
	}
	if b := body(nat); !strings.Contains(b, "Ports 80 and 443 forward to") || !strings.Contains(b, `href="/smoke/web/dev"`) || !strings.Contains(b, "home.example.com") {
		t.Errorf("nat drawer = %s", b)
	}
	nat.Forward, nat.NoForward = "", true
	if !strings.Contains(body(nat), "No port forward") {
		t.Error("nat drawer lacks the no-forward warning")
	}
}
