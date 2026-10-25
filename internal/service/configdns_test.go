package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A file that moves an env's ingress or pin lists the hosts that enter by
// another node: a managed host as a change row, a hand-managed one as a
// note naming the record to set. An ingress pick and a pin to an ingress
// node both move them.
func TestConfigDNSListsManagedAndHandHosts(t *testing.T) {
	ctx := context.Background()
	f := &zoneFake{}
	n := newTwoNodes(t, WithDNS(func(string, DNSCreds) (DNSProvider, error) { return f, nil }))
	ingressNode(t, n, "h4", "203.0.113.4")
	for id, ip := range map[string]string{n.ctl.ID: "198.51.100.1", "h4": "203.0.113.4"} {
		nd, err := n.st.Nodes.Get(ctx, id)
		must(t, err)
		nd.PublicIP = ip
		must(t, n.st.Nodes.Update(ctx, nd))
	}
	c, err := n.orch.AddConnector(ctx, ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: map[string]string{"user": "u", "key": "k", "client_ip": "203.0.113.7"}})
	must(t, err)
	_, err = n.orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil)
	must(t, err)
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d1", TileID: n.edgeT.ID, Host: "shop.acme.dev", Path: "/", CreatedAt: time.Now()}))
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d2", TileID: n.edgeT.ID, Host: "shop.example.com", Path: "/", CreatedAt: time.Now()}))
	addPlacement(t, n, "h4", "h4", true)

	for _, field := range []string{"ingress", "pin"} {
		w := promote.PlacementWrite{Scope: ScopeEnv, ScopeID: n.edge.ID, Field: field, Refs: []string{"h4"}}
		cs, notes, err := n.orch.configDNS(ctx, []promote.PlacementWrite{w})
		must(t, err)
		if len(cs) != 1 || cs[0].Kind != "dns" || cs[0].Field != "shop.acme.dev" || cs[0].Old != "198.51.100.1" || cs[0].New != "203.0.113.4" {
			t.Errorf("%s: changes = %+v", field, cs)
		}
		if len(notes) != 1 || !strings.Contains(notes[0], "shop.example.com -> A 203.0.113.4") {
			t.Errorf("%s: notes = %v", field, notes)
		}
	}

	// an unknown placement is the plan's blocker; no rows
	cs, notes, err := n.orch.configDNS(ctx, []promote.PlacementWrite{{Scope: ScopeEnv, ScopeID: n.edge.ID, Field: "pin", Refs: []string{"nowhere"}}})
	must(t, err)
	if len(cs) != 0 || len(notes) != 0 {
		t.Errorf("unknown placement: %+v %v", cs, notes)
	}
}

// A destination node with no public address gives a note naming the node,
// not a record row with nothing in it.
func TestConfigDNSNodeWithoutPublicIP(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	ingressNode(t, n, "h4", "")
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d2", TileID: n.edgeT.ID, Host: "shop.example.com", Path: "/", CreatedAt: time.Now()}))
	addPlacement(t, n, "h4", "h4", true)
	w := promote.PlacementWrite{Scope: ScopeEnv, ScopeID: n.edge.ID, Field: "ingress", Refs: []string{"h4"}}
	cs, notes, err := n.orch.configDNS(ctx, []promote.PlacementWrite{w})
	must(t, err)
	if len(cs) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "Node h4 has no public IP address") || strings.Contains(notes[0], "->") {
		t.Errorf("changes %+v notes %v", cs, notes)
	}
}
