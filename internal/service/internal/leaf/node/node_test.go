package node_test

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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
	if n.Role != node.Controller || n.RangeCIDR != "10.213.0.0/16" || n.Status != node.StatusOnline {
		t.Fatalf("controller row: %+v", n)
	}
	if got := node.Tags(n); !slices.Equal(got, []string{"ingress", "egress", "builder"}) {
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

func worker(t *testing.T, st *store.Store, name, rng string) store.Node {
	t.Helper()
	n := store.Node{
		ID: name, Name: name, Slug: name, Role: node.Worker, Tags: "[]",
		RangeCIDR: rng, Status: node.StatusOnline, CreatedAt: time.Now(),
	}
	if err := st.Nodes.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReserveUsesTheNodesRange(t *testing.T) {
	st := storetest.Store(t)
	l := node.New(st.Nodes)
	ctl, _, err := l.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w := worker(t, st, "w1", "10.209.0.0/16")

	p, err := l.Reserve(ctx, ctl.ID, nil)
	if err != nil || p.String() != "10.213.0.0/24" {
		t.Fatalf("controller block = %v, %v", p, err)
	}
	// The block just handed out is held even though nothing shows it yet.
	p, err = l.Reserve(ctx, ctl.ID, []netip.Prefix{netip.MustParsePrefix("10.213.1.0/24")})
	if err != nil || p.String() != "10.213.2.0/24" {
		t.Fatalf("second controller block = %v, %v", p, err)
	}
	// Another range than 10.213: the worker's own /16.
	p, err = l.Reserve(ctx, w.ID, []netip.Prefix{netip.MustParsePrefix("10.209.0.128/25")})
	if err != nil || p.String() != "10.209.1.0/24" {
		t.Fatalf("worker block = %v, %v", p, err)
	}
	if _, err := l.Reserve(ctx, "nope", nil); err == nil {
		t.Fatal("reserved on a missing node")
	}
}

func TestReserveExhausted(t *testing.T) {
	st := storetest.Store(t)
	l := node.New(st.Nodes)
	w := worker(t, st, "w1", "10.209.0.0/16")
	_, err := l.Reserve(ctx, w.ID, []netip.Prefix{netip.MustParsePrefix("10.209.0.0/16")})
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("full range: %v; want a Conflict", err)
	}
}
