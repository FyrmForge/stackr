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
			{ID: "n", Name: "nas", Role: "worker", Status: "online", Envs: envs("down")},
			{ID: "f", Name: "fsn1", Role: "worker", Status: "online", Public: []string{"203.0.113.5"},
				Tags: []string{"ingress"}, Domains: []string{"shop.example.com"}, ExitEnvs: []string{"acme/prod/web", "x/y/z"},
				Envs: envs("moving")},
			{ID: "g", Name: "fsn2", Role: "worker", Status: "unreachable"},
		},
		Nets: []cluster.Net{
			{ID: "192.168.1.0/24", CIDR: "192.168.1.0/24", Nodes: []string{"h", "n"}, Router: true},
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
		">NAT<", "192.168.1.0/24", "behind a router",
		"LAN · 0.3 ms", "internet · 21 ms",
		">private<",                      // no latency known: just the kind
		"home.example.com +2", "ingress", // ingress arrow labels
		"smoke/web/dev exits here", "acme/prod/web +1 exit here",
		"S3 · nightly",
		"+2 more", // five envs: three dealt, the rest counted
		"Not answering", "Healthy", "Moving",
		"Public IP", "203.0.113.5", "No public IP",
		`node-id="net:10.20.0.0/24"`, // a box drags by its handle
		`data-extent="`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// home deals three env sub-cards and the "+N more" one.
	if got := strings.Count(body, `node-id="env:h:`); got != 4 {
		t.Errorf("home has %d sub-cards, want 3 envs + more", got)
	}
}

// Cards and box handles never overlap.
func TestMapNoOverlap(t *testing.T) {
	v := cluster.Build(sample(), cluster.Base, nil, nil)
	for i, a := range v.Nodes {
		for _, b := range v.Nodes[i+1:] {
			if a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H {
				t.Errorf("%s and %s overlap", a.ID, b.ID)
			}
		}
	}
}

// A dropped box takes its cards along; a dropped card stays where it was
// put inside its box, and Drop turns a drop into what Placed keeps.
func TestMapPlaced(t *testing.T) {
	m := sample()
	pos := func(m cluster.Map, id string) (int, int) {
		for _, n := range cluster.Build(m, cluster.Base, nil, nil).Nodes {
			if n.ID == id {
				return n.X, n.Y
			}
		}
		t.Fatalf("no %s", id)
		return 0, 0
	}
	nx, ny := pos(m, "net:10.20.0.0/24")
	fx, fy := pos(m, "node:f")
	m.Placed = map[string][2]int{"net:10.20.0.0/24": {nx + 100, ny + 40}}
	if x, y := pos(m, "node:f"); x != fx+100 || y != fy+40 {
		t.Errorf("fsn1 at %d,%d after its box moved, want %d,%d", x, y, fx+100, fy+40)
	}
	v := cluster.Build(m, cluster.Base, nil, nil)
	p, ok := cluster.Drop(v, m, "node:f", nx+100+300, ny+40+400)
	if !ok || p != [2]int{300, 400} {
		t.Fatalf("Drop = %v %v, want 300,400 relative to the box", p, ok)
	}
	m.Placed["node:f"] = p
	if x, y := pos(m, "node:f"); x != nx+400 || y != ny+440 {
		t.Errorf("fsn1 at %d,%d, want where it was dropped", x, y)
	}
	if _, ok := cluster.Drop(v, m, "env:f:0", 0, 0); ok {
		t.Error("an env sub-card does not move")
	}
}
