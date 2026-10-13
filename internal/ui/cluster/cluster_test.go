package cluster_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/ui/cluster"
)

func envs(health ...string) []cluster.Env {
	var out []cluster.Env
	for i, h := range health {
		out = append(out, cluster.Env{Name: "acme/web/e" + string(rune('a'+i)), Href: "/acme/web/e", Health: h})
	}
	return out
}

func sample() cluster.Map {
	return cluster.Map{
		Nodes: []cluster.Node{
			{ID: "h", Name: "home", Role: "controller", Status: "online", Tags: []string{"ingress", "egress"},
				Domains: []string{"home.example.com", "a.example.com", "b.example.com"}, ExitEnvs: []string{"smoke/web/dev"},
				Envs: envs("healthy", "degraded", "healthy", "stopped", "healthy")},
			{ID: "n", Name: "nas", Role: "worker", Status: "online", Envs: envs("down"), Tags: []string{"ingress"}, NoForward: true},
			{ID: "f", Name: "fsn1", Role: "worker", Status: "online", Public: []string{"203.0.113.5"},
				Tags: []string{"ingress"}, Domains: []string{"shop.example.com"}, ExitEnvs: []string{"acme/prod/web", "x/y/z"},
				Envs: envs("moving")},
			{ID: "g", Name: "fsn2", Role: "worker", Status: "unreachable"},
		},
		Nets: []cluster.Net{
			{ID: "192.168.1.0/24", CIDR: "192.168.1.0/24", Nodes: []string{"h", "n"}, Router: true, Forward: "h"},
			{ID: "10.20.0.0/24", CIDR: "10.20.0.0/24", Nodes: []string{"f", "g"}},
		},
		Links: []cluster.Link{
			{A: "h", B: "n", Kind: "LAN", MS: 0.3},
			{A: "h", B: "f", Kind: "internet", MS: 21},
			{A: "f", B: "g", Kind: "private", MS: -1},
		},
		Backups: []cluster.Backup{{ID: "b", Name: "nightly", Nodes: []string{"n"}}},
	}
}

func render(t *testing.T) string {
	t.Helper()
	m := sample()
	var sb strings.Builder
	if err := cluster.Page(cluster.Build(m, cluster.Base, nil, nil), m).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func TestMapDraws(t *testing.T) {
	body := render(t)
	for _, want := range []string{
		"4 nodes · 2 networks",
		`node-id="node:h"`, `node-id="node:f"`,
		`hx-get="/-/admin/nodes/f"`, `hx-push-url="?drawer=node:f"`, // a node opens its drawer
		`node-id="nat:192.168.1.0/24"`, ">NAT<", "192.168.1.0/24", "behind a router",
		`node-id="internet"`, `node-id="s3:b"`, "S3 · nightly",
		`members="nat:192.168.1.0/24 node:h node:n"`, // a box holds its NAT and nodes
		"LAN · 0.3 ms", "internet · 21 ms",
		`data-edge-label>private<`, // no latency known: just the kind
		"home.example.com +2", // through the NAT: the box's ingress names
		"shop.example.com",    // straight to a public node
		"smoke/web/dev exits here", "acme/prod/web +1 exit here",
		"+2 more", // five envs: three listed, the rest counted
		"Not answering", "Healthy", "Moving",
		"Public IP", "203.0.113.5", "No public IP",
		"No port forward",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, want := range []string{
		`data-edge-kind="ingress" data-from="internet" data-to="nat:192.168.1.0/24"`,
		`data-edge-kind="forward" data-from="nat:192.168.1.0/24" data-to="node:h"`,
		`data-edge-kind="ingress" data-from="internet" data-to="node:f"`,
		`data-edge-kind="egress" data-from="node:f" data-to="internet"`,
		`data-edge-kind="tunnel" data-from="node:h" data-to="node:n"`,
		`data-edge-kind="backup" data-from="node:n" data-to="s3:b"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks edge %s", want)
		}
	}
	// home lists three envs and the "+N more" row inside its card.
	if got := strings.Count(body, `hx-trigger="click consume"`); got != 4+1+1 {
		t.Errorf("%d env rows, want home 4, nas 1, fsn1 1", got)
	}
}

// Cards never overlap each other, and every box holds its cards.
func TestMapNoOverlap(t *testing.T) {
	v := cluster.Build(sample(), cluster.Base, nil, nil)
	byID := map[string]int{}
	for i, n := range v.Nodes {
		byID[n.ID] = i
	}
	for i, a := range v.Nodes {
		for _, m := range a.Members {
			b := v.Nodes[byID[m]]
			if b.X < a.X || b.Y < a.Y || b.X+b.W > a.X+a.W || b.Y+b.H > a.Y+a.H {
				t.Errorf("%s is not inside %s", b.ID, a.ID)
			}
		}
		if len(a.Members) > 0 {
			continue
		}
		for _, b := range v.Nodes[i+1:] {
			if len(b.Members) == 0 && a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H {
				t.Errorf("%s and %s overlap", a.ID, b.ID)
			}
		}
	}
}

// A dropped card stays where it was put and its box wraps it; a box's own
// drop is not kept (it goes where its cards are).
func TestMapPlaced(t *testing.T) {
	m := sample()
	at := func(id string) (x, y, w, h int) {
		for _, n := range cluster.Build(m, cluster.Base, nil, nil).Nodes {
			if n.ID == id {
				return n.X, n.Y, n.W, n.H
			}
		}
		t.Fatalf("no %s", id)
		return
	}
	m.Placed = map[string][2]int{"node:f": {2000, 1500}}
	if x, y, _, _ := at("node:f"); x != 2000 || y != 1500 {
		t.Errorf("fsn1 at %d,%d, want 2000,1500", x, y)
	}
	bx, by, bw, bh := at("net:10.20.0.0/24")
	if bx > 2000 || by > 1500 || bx+bw < 2000+240 || by+bh < 1500+156 {
		t.Errorf("box %d,%d %dx%d does not hold fsn1", bx, by, bw, bh)
	}
	for id, want := range map[string]bool{"node:f": true, "nat:x": true, "internet": true, "s3:b": true, "net:x": false, "env:f:0": false} {
		if cluster.Movable(id) != want {
			t.Errorf("Movable(%s) = %v", id, !want)
		}
	}
}

// A network with one node draws no box; the node and its NAT stand alone.
func TestMapSingleNodeNetNoBox(t *testing.T) {
	m := sample()
	m.Nets[1].Nodes = []string{"f"}
	m.Nets[0].Nodes = []string{"h"}
	ids := map[string]bool{}
	for _, n := range cluster.Build(m, cluster.Base, nil, nil).Nodes {
		ids[n.ID] = true
	}
	for id, want := range map[string]bool{"net:10.20.0.0/24": false, "net:192.168.1.0/24": false, "node:f": true, "node:h": true, "nat:192.168.1.0/24": true} {
		if ids[id] != want {
			t.Errorf("%s drawn = %v, want %v", id, ids[id], want)
		}
	}
}

// Default spots: boxes stacked, one row each; the NAT, then the node it
// forwards to; the internet left of every box; tunnels fan out wider.
func TestMapLayout(t *testing.T) {
	v := cluster.Build(sample(), cluster.Base, nil, nil)
	at := map[string][4]int{}
	for _, n := range v.Nodes {
		at[n.ID] = [4]int{n.X, n.Y, n.W, n.H}
	}
	home, lan := at["net:192.168.1.0/24"], at["net:10.20.0.0/24"]
	if lan[1] < home[1]+home[3]+100 {
		t.Errorf("boxes not stacked with a gap: %v then %v", home, lan)
	}
	nat, h, n := at["nat:192.168.1.0/24"], at["node:h"], at["node:n"]
	if !(nat[0] < h[0] && h[0] < n[0]) || h[1] != n[1] {
		t.Errorf("row order NAT %v, forward target %v, rest %v", nat, h, n)
	}
	f, g := at["node:f"], at["node:g"]
	if f[0] >= g[0] || f[1] != g[1] {
		t.Errorf("public node %v not first in the row before %v", f, g)
	}
	in := at["internet"]
	for _, b := range [][4]int{home, lan} {
		if in[0]+in[2] >= b[0] {
			t.Errorf("internet %v not left of box %v", in, b)
		}
	}
	if v.FanGap == 0 {
		t.Error("cluster map keeps the default fan gap")
	}
}
