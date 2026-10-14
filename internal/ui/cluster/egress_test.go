package cluster_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/cluster"
	"github.com/FyrmForge/stackr/internal/ui/graph"
)

func kinds(v graph.View) map[string]bool {
	out := map[string]bool{}
	for _, e := range v.Edges {
		out[e.Kind+" "+e.From+" "+e.To] = true
	}
	return out
}

// Own-only egress is the thin look; an exit for others is the full one.
func TestEgressOwnVersusOthers(t *testing.T) {
	got := kinds(cluster.Build(sample(), cluster.Base, nil, nil))
	for _, want := range []string{
		"egress-own nat:192.168.1.0/24 internet:nat:192.168.1.0/24", // home: own apps only
		"egress node:f internet:node:f",                             // fsn1 is an exit for x/y/z
	} {
		if !got[want] {
			t.Errorf("no edge %q in %v", want, got)
		}
	}
	if got["egress nat:192.168.1.0/24 internet:nat:192.168.1.0/24"] {
		t.Error("an own-only NAT draws the full egress look")
	}
}

// The Hetzner shape: h4 is public, h3 sits in the same private network
// without an address. h3's own egress goes to one card shared by the network,
// and its envs that leave at h4 are an egress-via arrow to h4, which turns
// h4's internet arrow full.
func TestEgressNetworkGatewayAndVia(t *testing.T) {
	m := cluster.Map{
		Nodes: []cluster.Node{
			{ID: "h4", Name: "h4", Role: "worker", Status: "online", Public: []string{"203.0.113.4"}, Tags: []string{"egress"}, ExitOthers: []string{"acme/shop/dev"}},
			{ID: "h3", Name: "h3", Role: "worker", Status: "online", Tags: []string{"egress"}, ExitOwn: []string{"acme/api/dev"}},
			{ID: "h2", Name: "h2", Role: "worker", Status: "online", Tags: nil}, // nothing leaves here
		},
		Nets:   []cluster.Net{{ID: "10.0.0.0/16", CIDR: "10.0.0.0/16", Nodes: []string{"h4", "h3", "h2"}}},
		Egress: []cluster.Egress{{From: "h2", To: "h4", Envs: []string{"acme/shop/dev"}}},
	}
	v := cluster.Build(m, cluster.Base, nil, nil)
	got := kinds(v)
	for _, want := range []string{
		"egress-own node:h3 internet:net:10.0.0.0/16",
		"egress node:h4 internet:node:h4",
		"egress-via node:h2 node:h4",
	} {
		if !got[want] {
			t.Errorf("no edge %q in %v", want, got)
		}
	}
	at := map[string][4]int{}
	for _, n := range v.Nodes {
		at[n.ID] = [4]int{n.X, n.Y, n.W, n.H}
	}
	if _, ok := at["internet:node:h3"]; ok {
		t.Error("h3 still has an internet card of its own")
	}
	in, box := at["internet:net:10.0.0.0/16"], at["net:10.0.0.0/16"]
	if in[2] == 0 || in[1]+in[3] > box[1] {
		t.Errorf("shared internet card %v is not above its box %v", in, box)
	}
	body := render1(t, m, v)
	if !strings.Contains(body, `hx-get="/-/cluster/internet?from=net%3A10.0.0.0%2F16"`) {
		t.Error("the shared card opens no drawer")
	}

	info, ok := cluster.Internet(m, "net:10.0.0.0/16")
	if !ok || !info.Gateway || len(info.EgressOwn) != 1 || len(info.EgressOthers) != 0 {
		t.Errorf("gateway card = %+v ok %v", info, ok)
	}
	if _, ok := cluster.Internet(m, "node:h3"); ok {
		t.Error("h3 has a card of its own")
	}
	var sb strings.Builder
	if err := cluster.InternetBody(info).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Leaves through the network's gateway.", "Own apps", `href="/acme/api/dev"`} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("gateway drawer lacks %q:\n%s", want, sb.String())
		}
	}
	if strings.Contains(sb.String(), "Ingress") || strings.Contains(sb.String(), "Exit for other nodes") {
		t.Error("gateway drawer lists ingress or an empty exit section")
	}
	h4, _ := cluster.Internet(m, "node:h4")
	sb.Reset()
	_ = cluster.InternetBody(h4).Render(context.Background(), &sb)
	if !strings.Contains(sb.String(), "Exit for other nodes") || !strings.Contains(sb.String(), `href="/acme/shop/dev"`) {
		t.Error("h4's drawer lacks the envs it is an exit for")
	}
}

func render1(t *testing.T, m cluster.Map, v graph.View) string {
	t.Helper()
	var sb strings.Builder
	if err := cluster.Page(v, m).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}
