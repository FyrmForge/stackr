package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// publicIPs gives the controller and w1 their public addresses.
func publicIPs(t *testing.T, n *twoNodes, ctl, w1 string) {
	t.Helper()
	ctx := context.Background()
	for id, ip := range map[string]string{n.ctl.ID: ctl, "w1": w1} {
		nd, err := n.st.Nodes.Get(ctx, id)
		must(t, err)
		nd.PublicIP = ip
		must(t, n.st.Nodes.Update(ctx, nd))
	}
}

func recordsOf(h DNSHost) []string {
	var out []string
	for _, r := range h.Records {
		out = append(out, r.Type+" "+r.Name+" "+r.Value)
	}
	return out
}

// An env move and a pin move to a node that enters by its own proxy list the
// env's hand-managed hosts with the records they need; a host that does not
// change ingress is not listed, and the IPv6 row follows the node's address.
func TestMovePlanListsHandManagedHostsAndRecords(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	publicIPs(t, n, "198.51.100.1", "198.51.100.2")
	addPlacement(t, n, "w1", "w1", true)
	mustTag(t, n, adm, "w1", "ingress")
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d1", TileID: n.devT.ID, Host: "shop.example.com", Path: "/", CreatedAt: time.Now()}))

	plan, err := n.orch.PlanEnvPin(ctx, n.dev.ID, "w1")
	must(t, err)
	if len(plan.DNS.Hand) != 1 || len(plan.DNS.Managed) != 0 {
		t.Fatalf("env move dns = %+v", plan.DNS)
	}
	if got := recordsOf(plan.DNS.Hand[0]); !slices.Equal(got, []string{"A shop.example.com 198.51.100.2"}) {
		t.Errorf("records = %v", got)
	}
	// the pin move of the whole stack lists the same host
	_, plans, err := n.orch.pinPlans(ctx, ScopeStack, n.stack, mustPlacement(t, n, "w1"), nil)
	must(t, err)
	var hosts []string
	for _, p := range plans {
		hosts = append(hosts, p.DNS.HandHosts()...)
	}
	if !slices.Equal(hosts, []string{"shop.example.com"}) {
		t.Errorf("pin move hosts = %v", hosts)
	}

	// IPv6 only when the node has one public
	publicIPs(t, n, "198.51.100.1", "2001:db8::2")
	plan, err = n.orch.PlanEnvPin(ctx, n.dev.ID, "w1")
	must(t, err)
	if got := recordsOf(plan.DNS.Hand[0]); !slices.Equal(got, []string{"AAAA shop.example.com 2001:db8::2"}) {
		t.Errorf("ipv6 records = %v", got)
	}
}

// A managed host is listed apart with its old and new address and the
// connector; ticking nothing off holds none, unticking holds it.
func TestMovePlanListsManagedHostsAndUntickHolds(t *testing.T) {
	ctx := context.Background()
	f := &zoneFake{}
	n := newTwoNodes(t, WithDNS(func(string, DNSCreds) (DNSProvider, error) { return f, nil }))
	adm := adminP()
	publicIPs(t, n, "198.51.100.1", "198.51.100.2")
	c, err := n.orch.AddConnector(ctx, ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "u", "key": "k", "client_ip": "203.0.113.7"}})
	must(t, err)
	_, err = n.orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil)
	must(t, err)
	_, err = n.orch.AttachDomain(ctx, n.devT.ID, DomainSpec{Host: "app.acme.dev"})
	must(t, err)
	addPlacement(t, n, "w1", "w1", true)
	mustTag(t, n, adm, "w1", "ingress")

	plan, err := n.orch.PlanEnvPin(ctx, n.dev.ID, "w1")
	must(t, err)
	if len(plan.DNS.Hand) != 0 || len(plan.DNS.Managed) != 1 {
		t.Fatalf("dns = %+v", plan.DNS)
	}
	m := plan.DNS.Managed[0]
	if m.Host != "app.acme.dev" || !slices.Equal(m.Old, []string{"198.51.100.1"}) || !slices.Equal(m.New, []string{"198.51.100.2"}) || m.Connector != c.Connector.Name {
		t.Errorf("managed = %+v", m)
	}

	must(t, n.orch.keepManaged(ctx, plan.DNS)) // ticked: nothing held
	if got := n.orch.DNSHeld(ctx); len(got) != 0 {
		t.Errorf("held after a ticked move = %v", got)
	}
	must(t, n.orch.keepManaged(KeepDNS(ctx), plan.DNS)) // unticked
	if got := n.orch.DNSHeld(ctx); !slices.Equal(got, []string{"app.acme.dev"}) {
		t.Errorf("held after an unticked move = %v", got)
	}
	must(t, n.orch.ReleaseDNS(ctx, []string{"app.acme.dev"}))
	if got := n.orch.DNSHeld(ctx); len(got) != 0 {
		t.Errorf("held after release = %v", got)
	}
}

// The live check marks a host that resolves to what the move wants and keeps
// the addresses of one that does not; a host that does not resolve says so.
func TestCheckDNSReportsMatch(t *testing.T) {
	n := newTwoNodes(t)
	fakeDNS(t, map[string][]string{
		"ok.example.com":  {"198.51.100.2"},
		"old.example.com": {"198.51.100.1"},
	})
	m := n.orch.CheckDNS(context.Background(), DNSMove{Hand: []DNSHost{
		{Host: "ok.example.com", Want: []string{"198.51.100.2"}},
		{Host: "old.example.com", Want: []string{"198.51.100.2"}},
		{Host: "gone.example.com", Want: []string{"198.51.100.2"}},
	}})
	ok, old, gone := m.Hand[0], m.Hand[1], m.Hand[2]
	if !ok.OK || old.OK || gone.OK {
		t.Errorf("ok = %v %v %v", ok.OK, old.OK, gone.OK)
	}
	if !slices.Equal(old.Points, []string{"198.51.100.1"}) || gone.Error == "" {
		t.Errorf("old = %+v gone = %+v", old, gone)
	}
}
