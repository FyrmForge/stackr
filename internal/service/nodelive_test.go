package service

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/worker"
)

func TestGroupIfaces(t *testing.T) {
	f := worker.Facts{Interfaces: []worker.Iface{
		{Name: "lo", Prefixes: []string{"127.0.0.1/8", "::1/128"}},
		{Name: "eth0", Prefixes: []string{"203.0.113.7/24", "fe80::1/64"}, Public: true},
		{Name: "enp7s0", Prefixes: []string{"192.168.1.9/24", "100.64.1.2/32"}},
		{Name: "stackr0", Prefixes: []string{"10.213.0.2/16"}},
		{Name: "br-1a2b", Prefixes: []string{"10.208.3.1/24"}},
		{Name: "br-job", Prefixes: []string{"10.209.7.1/24"}},
		{Name: "br-other", Prefixes: []string{"172.18.0.1/16"}},
		{Name: "docker0", Prefixes: []string{"172.17.0.1/16"}},
		{Name: "veth9", Prefixes: []string{"10.208.3.9/24"}},
	}}
	got := groupIfaces(f, map[string]string{"10.208.3.0/24": "acme/shop/prod"})
	type row struct{ Group, Name, Label string }
	var rows []row
	for _, g := range got {
		rows = append(rows, row{g.Group, g.Name, g.Label})
	}
	want := []row{
		{IfaceHidden, "lo", "loopback"},
		{IfaceHidden, "lo", "loopback"},
		{IfacePublic, "eth0", ""},
		{IfaceHidden, "eth0", "link-local"},
		{IfaceLAN, "enp7s0", ""},
		{IfaceLAN, "enp7s0", ""}, // CGNAT is not public
		{IfaceCluster, "stackr0", "tunnel"},
		{IfaceCluster, "br-1a2b", "acme/shop/prod"},
		{IfaceCluster, "br-job", "stackr network"},
		{IfaceDocker, "br-other", ""},
		{IfaceDocker, "docker0", ""},
		{IfaceHidden, "veth9", "veth"},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows = %+v\nwant   %+v", rows, want)
	}
}

func TestNodeLive(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.mesh.Report(ctx, "w1", worker.Facts{Interfaces: []worker.Iface{{Name: "eth0", Prefixes: []string{"203.0.113.7/24"}}}})
	l, err := n.orch.NodeLive(ctx, "w1")
	must(t, err)
	if l.LastSeen.IsZero() || l.Facts == nil || len(l.Ifaces) != 1 || l.Ifaces[0].Group != IfacePublic {
		t.Fatalf("w1 = %+v", l)
	}
	c, err := n.orch.NodeLive(ctx, n.ctl.ID)
	must(t, err)
	if c.LastSeen.IsZero() || c.Facts == nil {
		t.Fatalf("controller = %+v", c)
	}
	if _, err := n.orch.NodeLive(ctx, "nope"); err == nil {
		t.Error("unknown node found")
	}
}
