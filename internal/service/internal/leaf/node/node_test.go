package node_test

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

var ctx = context.Background()

func TestBootstrapMakesOneController(t *testing.T) {
	st := storetest.Store(t)
	l := node.New(st.Nodes)
	n, created, err := l.Bootstrap(ctx)
	if err != nil || !created {
		t.Fatalf("first boot: created=%v err=%v", created, err)
	}
	if n.Role != node.Controller || n.WGAddress != "10.207.255.6" || n.Status != node.StatusOnline {
		t.Fatalf("controller row: %+v", n)
	}
	if got := node.Tags(n); !slices.Equal(got, []string{"ingress", "egress", "builder", "storage"}) {
		t.Fatalf("tags = %v", got)
	}
	// A second boot, and a lazy Controller, find the same row.
	again, created, err := l.Bootstrap(ctx)
	if err != nil || created || again.ID != n.ID {
		t.Fatalf("second boot: %+v created=%v err=%v", again, created, err)
	}
	if c, err := node.New(st.Nodes).Controller(ctx); err != nil || c.ID != n.ID {
		t.Fatalf("Controller = %+v, %v", c, err)
	}
	all, err := l.List(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("nodes = %d, %v; want 1", len(all), err)
	}
}

// One pool for the cluster: blocks come from 10.208.0.0/12 whichever node
// asks, skip what the caller uses, what Taken reports (other nodes' networks)
// and what was just handed out on any node.
func TestReservePool(t *testing.T) {
	st := storetest.Store(t)
	l := node.New(st.Nodes)
	other := []netip.Prefix{netip.MustParsePrefix("10.208.1.0/24")}
	l.Taken = func(context.Context) ([]netip.Prefix, error) { return other, nil }
	p, err := l.Reserve(ctx, "ctl", nil)
	if err != nil || p.String() != "10.208.0.0/24" {
		t.Fatalf("first block = %v, %v", p, err)
	}
	// Held for the next caller, on another node; Taken's block is skipped.
	p, err = l.Reserve(ctx, "w1", []netip.Prefix{netip.MustParsePrefix("10.208.2.0/24")})
	if err != nil || p.String() != "10.208.3.0/24" {
		t.Fatalf("second block = %v, %v", p, err)
	}
}

func TestReserveExhausted(t *testing.T) {
	st := storetest.Store(t)
	l := node.New(st.Nodes)
	_, err := l.Reserve(ctx, "w1", []netip.Prefix{netip.MustParsePrefix("10.208.0.0/12")})
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("full pool: %v; want a Conflict", err)
	}
}

// Confirm hands out the lowest free transit address, skipping the
// controller's; a rejoin keeps its own.
func TestConfirmTransit(t *testing.T) {
	st := storetest.Store(t)
	l := node.New(st.Nodes)
	if _, _, err := l.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := l.CreatePending(ctx, "w1", nil, "k", true)
	if err != nil {
		t.Fatal(err)
	}
	n, err := l.Confirm(ctx, p.ID, []netip.Prefix{netip.MustParsePrefix("192.168.1.5/24")})
	if err != nil || n.WGAddress != "10.207.255.1" || n.Status != node.StatusJoined {
		t.Fatalf("confirm = %+v, %v", n, err)
	}
	p2, _ := l.CreatePending(ctx, "w2", nil, "k2", true)
	if _, err := l.Confirm(ctx, p2.ID, []netip.Prefix{netip.MustParsePrefix("10.208.4.1/24")}); err == nil {
		t.Fatal("a node on the cluster pool was confirmed")
	}
}
