package environment_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/fakehost"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

func ptr(s string) *string { return &s }

// An env's own pin beats its stack's, which beats its org's, which beats the
// default placement; the empty string is no pin.
func TestPlace(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		pins environment.Pins
		want string
	}{
		"nothing pinned": {"", environment.Pins{Default: "c"}, "c"},
		"org":            {"", environment.Pins{Org: ptr("o"), Default: "c"}, "o"},
		"stack over org": {"", environment.Pins{Stack: ptr("s"), Org: ptr("o"), Default: "c"}, "s"},
		"env over all":   {"e", environment.Pins{Stack: ptr("s"), Org: ptr("o"), Default: "c"}, "e"},
		"empty pin":      {"", environment.Pins{Stack: ptr(""), Org: ptr("o"), Default: "c"}, "o"},
	} {
		if got := environment.Place(tc.env, tc.pins); got != tc.want {
			t.Errorf("%s: placed on %q, want %q", name, got, tc.want)
		}
	}
}

// Create reads the pins through the leaf's hook; an env made earlier never
// moves when a pin changes, and a PR env stays on its base env's node.
func TestCreatePlacesByPins(t *testing.T) {
	st := storetest.Store(t)
	ctl := storetest.Controller(t, st)
	now := time.Now()
	for _, id := range []string{"w1", "w2"} {
		if err := st.Nodes.Create(ctx, store.Node{
			ID: id, Name: id, Slug: id, Role: node.Worker, Tags: "[]",
			RangeCIDR: map[string]string{"w1": "10.209.0.0/16", "w2": "10.210.0.0/16"}[id],
			Status:    node.StatusOnline, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.Placements.Create(ctx, store.Placement{ID: "p-" + id, Name: id, NodeID: id, OpenToAll: true, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	stack := seedStack(t, st)
	stk, err := st.Stacks.Get(ctx, stack)
	if err != nil {
		t.Fatal(err)
	}
	def, err := st.Placements.GetByName(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	nodeOf := func(ctx context.Context, id string) (string, error) {
		p, err := st.Placements.Get(ctx, id)
		return p.NodeID, err
	}
	pins := func(context.Context, string) (environment.Pins, error) {
		s, err := st.Stacks.Get(ctx, stack)
		if err != nil {
			return environment.Pins{}, err
		}
		o, err := st.Orgs.Get(ctx, stk.OrgID)
		if err != nil {
			return environment.Pins{}, err
		}
		return environment.Pins{Stack: s.PlacementID, Org: o.PlacementID, Default: def.ID, NodeOf: nodeOf}, nil
	}
	l := environment.New(st.Environments, dockerfake.New()).WithPlacement(pins)

	dev, err := l.Create(ctx, stack, "dev", branch("main"))
	if err != nil || dev.NodeID != ctl.ID || dev.PlacementID != def.ID {
		t.Fatalf("no pins: %q in %q, %v; want the controller in default", dev.NodeID, dev.PlacementID, err)
	}

	org, _ := st.Orgs.Get(ctx, stk.OrgID)
	org.PlacementID = ptr("p-w1")
	if err := st.Orgs.Update(ctx, org); err != nil {
		t.Fatal(err)
	}
	qa, _ := l.Create(ctx, stack, "qa", branch("qa"))
	if qa.NodeID != "w1" || qa.PlacementID != "p-w1" {
		t.Errorf("org pinned to w1: env on %q in %q", qa.NodeID, qa.PlacementID)
	}

	stk.PlacementID = ptr("p-w2")
	if err := st.Stacks.Update(ctx, stk); err != nil {
		t.Fatal(err)
	}
	prod, _ := l.Create(ctx, stack, "prod", branch("prod"))
	if prod.NodeID != "w2" {
		t.Errorf("stack pinned to w2: env on %q", prod.NodeID)
	}
	spec := branch("edge")
	spec.NodeID = ctl.ID
	edge, _ := l.Create(ctx, stack, "edge", spec)
	if edge.NodeID != ctl.ID {
		t.Errorf("env pinned to the controller: on %q", edge.NodeID)
	}

	// Changing a pin never moves an env that exists.
	for _, e := range []store.Environment{dev, qa} {
		got, err := l.Get(ctx, e.ID)
		if err != nil || got.NodeID != e.NodeID {
			t.Errorf("%s moved to %q after the pins changed, %v", e.Slug, got.NodeID, err)
		}
	}
	// A whole-row Update from a stale copy cannot move it either.
	stale := qa
	stale.NodeID, stale.Subnet = ctl.ID, "10.213.9.0/24"
	if err := st.Environments.Update(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get(ctx, qa.ID); got.NodeID != "w1" || got.Subnet != "" {
		t.Errorf("Update wrote node_id/subnet: %+v", got)
	}

	// A PR env is made where its base env runs.
	pr, err := l.CloneRow(ctx, qa, "pr-1", "feature")
	if err != nil || pr.NodeID != "w1" {
		t.Errorf("PR env of an env on w1: on %q, %v", pr.NodeID, err)
	}
}

// A network is made on a /24 of the cluster pool, written on the env row
// first, whichever node the env is on: blocks are unique across nodes.
func TestNetworkReservesFromThePool(t *testing.T) {
	st := storetest.Store(t)
	nodes := node.New(st.Nodes)
	ctl := storetest.Controller(t, st)
	if err := st.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16", WGAddress: "10.207.255.2",
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	host, err := fakehost.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := environment.New(st.Environments, host).WithSubnets(nodes)
	stack := seedStack(t, st)

	mk := func(name, nodeID string) store.Environment {
		t.Helper()
		sp := branch("main")
		sp.NodeID = nodeID
		e, err := l.Create(ctx, stack, name, sp)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	subnet := func(e store.Environment) string {
		t.Helper()
		got, err := l.Get(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Subnet
	}
	a, b, c := mk("a", "w1"), mk("b", "w1"), mk("c", ctl.ID)
	for _, e := range []store.Environment{a, b, c} {
		if _, err := l.Network(docker.WithNode(ctx, "other"), e); err != nil { // the env's node wins over ctx
			t.Fatal(err)
		}
	}
	if got, want := [3]string{subnet(a), subnet(b), subnet(c)}, [3]string{"10.208.0.0/24", "10.208.1.0/24", "10.208.2.0/24"}; got != want {
		t.Fatalf("subnets = %v, want %v", got, want)
	}
	// Making it again keeps the subnet and creates nothing new.
	again, _ := l.Get(ctx, a.ID)
	if _, err := l.Network(ctx, again); err != nil || subnet(a) != "10.208.0.0/24" {
		t.Fatalf("second Network: %q, %v", subnet(a), err)
	}

	// A shared network takes the pool's next block too (made on the node in ctx).
	if err := l.Shared(docker.WithNode(ctx, "w1"), "stackr-shared-x"); err != nil {
		t.Fatal(err)
	}
	used, _ := host.UsedSubnets(ctx)
	if !hasPrefix(used, "10.208.3.0/24") {
		t.Fatalf("shared network not on the next block: %v", used)
	}
	// No node in ctx for a network that must be made is an error, not a guess.
	if err := l.Shared(ctx, "stackr-shared-y"); !errors.Is(err, docker.ErrNoNode) {
		t.Fatalf("Shared without a node = %v, want ErrNoNode", err)
	}
}

func hasPrefix(ps []netip.Prefix, s string) bool {
	for _, p := range ps {
		if p.String() == s {
			return true
		}
	}
	return false
}

// A pin on a placement with no node yet makes the env with no node: it
// waits (D6) instead of landing on the controller.
func TestCreateOnNodelessPlacement(t *testing.T) {
	st := storetest.Store(t)
	storetest.Controller(t, st)
	if err := st.Placements.Create(ctx, store.Placement{ID: "p-wait", Name: "wait", OpenToAll: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stack := seedStack(t, st)
	stk, _ := st.Stacks.Get(ctx, stack)
	stk.PlacementID = ptr("p-wait")
	if err := st.Stacks.Update(ctx, stk); err != nil {
		t.Fatal(err)
	}
	def, _ := st.Placements.GetByName(ctx, "default")
	l := environment.New(st.Environments, dockerfake.New()).WithPlacement(func(context.Context, string) (environment.Pins, error) {
		return environment.Pins{Stack: stk.PlacementID, Default: def.ID, NodeOf: func(ctx context.Context, id string) (string, error) {
			p, err := st.Placements.Get(ctx, id)
			return p.NodeID, err
		}}, nil
	})
	e, err := l.Create(ctx, stack, "dev", branch("main"))
	if err != nil || e.NodeID != "" || e.PlacementID != "p-wait" {
		t.Fatalf("env on a nodeless placement: node %q placement %q, %v", e.NodeID, e.PlacementID, err)
	}
}
