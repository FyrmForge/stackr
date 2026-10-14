package service

import (
	"context"
	"errors"
	"slices"
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
	if _, err := n.orch.IngressSwitchPlan(ctx, adm, "h4"); !isConflict(err) {
		t.Errorf("offline = %v, want a conflict", err)
	}
	if _, err := n.orch.SetDefaultIngress(ctx, adm, "h4", "h4"); !isConflict(err) {
		t.Errorf("offline set = %v, want a conflict", err)
	}
	if _, err := n.orch.IngressSwitchPlan(ctx, adm, "plain"); err == nil {
		t.Error("a node without the ingress tag got a plan")
	}
	if _, err := n.orch.IngressSwitchPlan(ctx, nil, "h4"); err == nil {
		t.Error("a nil principal got a plan")
	}

	// no public IP and no port forward: the internet cannot reach it
	if err := ingressReachable(ClusterNode{Name: "h3", NoForward: true}); !isConflict(err) {
		t.Errorf("no public address and no forward = %v, want a conflict", err)
	}
	for _, c := range []ClusterNode{
		{Name: "h4", Public: []string{"203.0.113.4"}, NoForward: true}, // public wins
		{Name: "home"}, // the forward target, or unsure
	} {
		if err := ingressReachable(c); err != nil {
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
