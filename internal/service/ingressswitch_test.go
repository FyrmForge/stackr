package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// fakeDNS answers resolveHost from a table; a host it does not know fails.
func fakeDNS(t *testing.T, table map[string][]string) {
	t.Helper()
	old := resolveHost
	resolveHost = func(_ context.Context, host string) ([]string, error) {
		if ips, ok := table[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { resolveHost = old })
}

// ingressNode is an online ingress worker whose public address the map
// remembers.
func ingressNode(t *testing.T, n *twoNodes, id, public string) {
	t.Helper()
	seedNode(t, n, id, `["ingress"]`, "10.240."+id[len(id)-1:]+".0/24")
	if public != "" {
		must(t, n.st.Settings.Set(context.Background(), memoryKey(id), `{"net":"public:`+id+`","public":["`+public+`"]}`))
	}
}

// The plan lists the panel domain first, then the routes nobody picked an
// ingress for (external routes, tile domains without a pick), and says for
// each whether it already points at the node.
func TestIngressSwitchPlanListsPanelFirstAndChecksDNS(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	ingressNode(t, n, "h4", "203.0.113.4")
	must(t, n.st.Settings.Set(ctx, "panel_domain", "panel.example.com"))
	must(t, n.st.Routes.Create(ctx, store.Route{ID: "r1", Host: "*.apps.example.com", Mode: "http", Target: "10.0.0.9:80", CreatedAt: time.Now()}))
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d1", TileID: n.edgeT.ID, Host: "shop.example.com", Path: "/", CreatedAt: time.Now()}))
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d2", TileID: n.devT.ID, Host: "picked.example.com", Path: "/", CreatedAt: time.Now()}))
	must(t, n.st.IngressPicks.Set(ctx, ScopeTile, n.devT.ID, "w1")) // this one enters elsewhere
	fakeDNS(t, map[string][]string{
		"panel.example.com":             {"198.51.100.9"},
		"stackr-probe.apps.example.com": {"203.0.113.4"},
		"shop.example.com":              {"203.0.113.4", "198.51.100.9"},
	})

	plan, err := n.orch.IngressSwitchPlan(ctx, adminP(), "h4")
	must(t, err)
	if plan.Node != "h4" || !plan.Known || !slices.Equal(plan.Addrs, []string{"203.0.113.4"}) {
		t.Fatalf("plan = %+v", plan)
	}
	var hosts []string
	for _, h := range plan.Hosts {
		hosts = append(hosts, h.Host)
	}
	if !slices.Equal(hosts, []string{"panel.example.com", "*.apps.example.com", "shop.example.com"}) {
		t.Fatalf("hosts = %v; want the panel first, the wildcard route and the unpicked tile domain, not the picked one", hosts)
	}
	got := map[string]IngressHost{}
	for _, h := range plan.Hosts {
		got[h.Host] = h
	}
	if p := got["panel.example.com"]; !p.Panel || p.OK || !slices.Equal(p.Points, []string{"198.51.100.9"}) {
		t.Errorf("panel = %+v, want flagged and not OK", p)
	}
	if !got["*.apps.example.com"].OK {
		t.Errorf("wildcard = %+v, want OK through the probe name", got["*.apps.example.com"])
	}
	if got["shop.example.com"].OK {
		t.Error("a host that also points elsewhere was marked OK")
	}
}

// A node whose address is its NAT's is Known=false: nothing is marked OK.
func TestIngressSwitchPlanWithoutAPublicAddress(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	ingressNode(t, n, "h4", "")
	must(t, n.st.Settings.Set(ctx, "panel_domain", "panel.example.com"))
	fakeDNS(t, map[string][]string{"panel.example.com": {"198.51.100.9"}})
	plan, err := n.orch.IngressSwitchPlan(ctx, adminP(), "h4")
	must(t, err)
	if plan.Known || len(plan.Hosts) != 1 || plan.Hosts[0].OK {
		t.Errorf("plan = %+v", plan)
	}
}

func TestIngressSwitchRefusals(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	ingressNode(t, n, "h4", "203.0.113.4")
	seedNode(t, n, "plain", `[]`, "10.241.0.0/24")
	h4, err := n.st.Nodes.Get(ctx, "h4")
	must(t, err)
	h4.Status = "offline"
	must(t, n.st.Nodes.Update(ctx, h4))
	if _, err := n.orch.IngressSwitchPlan(ctx, adm, "h4"); !isNodeDown(err) {
		t.Errorf("offline = %v, want node down", err)
	}
	if _, err := n.orch.SetDefaultIngress(ctx, adm, "h4", "h4"); !isNodeDown(err) {
		t.Errorf("offline set = %v, want node down", err)
	}
	if _, err := n.orch.IngressSwitchPlan(ctx, adm, "plain"); err == nil {
		t.Error("a node without the ingress tag got a plan")
	}
	if _, err := n.orch.IngressSwitchPlan(ctx, nil, "h4"); err == nil {
		t.Error("a nil principal got a plan")
	}

	// no public IP and no port forward: the internet cannot reach it
	m := ClusterMap{
		Nodes: []ClusterNode{{ID: "ctl", Name: "ctl"}},
		Networks: []ClusterNet{
			{ID: "lan", Router: true, Forward: "ctl"},
			{ID: "hetzner"}, // h4 has the public address, h3 none: no router
			{ID: "unsure", Router: true},
			{ID: "unknown"},
		},
	}
	for _, c := range []ClusterNode{
		{Name: "h3", NoForward: true},
		{ID: "h3", Name: "h3", Net: "hetzner"},
		{ID: "ct110", Name: "ct110", Net: "lan"},
	} {
		if err := ingressReachable(m, c); !isConflict(err) {
			t.Errorf("%s = %v, want a conflict", c.Name, err)
		}
	}
	if err := ingressReachable(m, ClusterNode{ID: "ct110", Name: "ct110", Net: "lan"}); err == nil || !strings.Contains(err.Error(), "forwards to ctl") {
		t.Errorf("lan refusal = %v, want it to name the forward", err)
	}
	for _, c := range []ClusterNode{
		{Name: "h4", Public: []string{"203.0.113.4"}, NoForward: true, Net: "hetzner"}, // public wins
		{ID: "ctl", Name: "ctl", Net: "lan"},                                           // the forward target
		{ID: "x", Name: "x", Net: "unsure"},                                            // nobody answered
		{Name: "home"},                                                                 // not on the map
		{Name: "new", Net: "unknown"},                                                  // no facts yet
	} {
		if err := ingressReachable(m, c); err != nil {
			t.Errorf("%s refused: %v", c.Name, err)
		}
	}
}

// The name must be typed; the right one moves the default.
func TestSetDefaultIngressNeedsTheNodesName(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	ingressNode(t, n, "h4", "203.0.113.4")
	nodeFake(t, n, "h4")
	n.orch.stackrPublished = nil // the proxy sync after the switch must not wait for a registry
	if _, err := n.orch.SetDefaultIngress(ctx, adm, "h4", "h3"); err == nil {
		t.Fatal("a wrong confirm moved the default ingress")
	}
	if id, _ := n.orch.DefaultIngress(ctx); id == "h4" {
		t.Fatal("the setting changed anyway")
	}
	d, err := n.orch.SetDefaultIngress(ctx, adm, "h4", "h4")
	must(t, err)
	if d.Ingress != "h4" {
		t.Errorf("defaults = %+v", d)
	}
}

// A tile domain in a managed domain is left out of the plan: stackr
// repoints its records itself, so its DNS never holds the switch up.
func TestIngressSwitchPlanSkipsManagedHosts(t *testing.T) {
	ctx := context.Background()
	f := &zoneFake{}
	n := newTwoNodes(t, WithDNS(func(string, DNSCreds) (DNSProvider, error) { return f, nil }))
	ingressNode(t, n, "h4", "203.0.113.4")
	c, err := n.orch.AddDNSConnector(ctx, nil, DNSNamecheap, DNSAccount{User: "u", Key: "k", ClientIP: "203.0.113.7"})
	must(t, err)
	_, err = n.orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil)
	must(t, err)
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d1", TileID: n.edgeT.ID, Host: "shop.acme.dev", Path: "/", CreatedAt: time.Now()}))
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d2", TileID: n.edgeT.ID, Host: "shop.example.com", Path: "/", CreatedAt: time.Now()}))
	fakeDNS(t, map[string][]string{"shop.example.com": {"203.0.113.4"}})

	plan, err := n.orch.IngressSwitchPlan(ctx, adminP(), "h4")
	must(t, err)
	if len(plan.Hosts) != 1 || plan.Hosts[0].Host != "shop.example.com" {
		t.Errorf("hosts = %+v, want only the hand-pointed one", plan.Hosts)
	}
}
