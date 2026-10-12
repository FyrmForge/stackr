package service

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// A consumer on w1 of a postgres instance on the controller: an ExtraHosts
// line to the instance's VIP, an Out flow on w1, an accept on the controller.
// A consumer on the instance's own node keeps the network.
func TestCrossNodeLink(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	m, err := managed.New(n.st.ManagedInstances, n.st.Provisions, n.st.Bindings).Create(ctx, n.devT.ID, "postgres", "stackr", "")
	must(t, err)
	from := "shop:dev:" + n.devT.Slug
	sl, err := n.orch.tiles.Create(ctx, store.Tile{StackID: n.stack, EnvironmentID: n.edge.ID, Name: "db", Kind: tile.Slice, ProvisionFrom: &from})
	must(t, err)
	p := store.Provision{ID: uuid.NewString(), TileID: sl.ID, InstanceID: m.ID, DBName: "a", DBUser: "a", DBPassword: "x", CreatedAt: time.Now()}
	must(t, n.st.Provisions.Create(ctx, p))
	must(t, n.st.Bindings.Create(ctx, store.Binding{ID: uuid.NewString(), ProvisionID: p.ID, ConsumerTileID: n.edgeT.ID,
		Access: tile.Write, DBUser: "b", DBPassword: "y", Outputs: "{}", CreatedAt: time.Now()}))
	n.ctlFake.Containers = append(n.ctlFake.Containers, docker.Container{ID: "p", State: "running", IPs: []string{"10.208.1.2"},
		Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "pause"}})

	edge, err := n.st.Environments.Get(ctx, n.edge.ID)
	must(t, err)
	edge.Subnet = "10.209.1.0/24"
	must(t, n.st.Environments.SetSubnet(ctx, edge.ID, edge.Subnet))
	dev, err := n.st.Environments.Get(ctx, n.dev.ID)
	must(t, err)
	if h, err := n.orch.linkHost(ctx, edge, m.ID); err != nil || h != managed.Alias(n.devT.Slug, m.ID)+":10.208.1.2" {
		t.Errorf("host from w1 = %q %v", h, err)
	}
	if h, err := n.orch.linkHost(ctx, dev, m.ID); err != nil || h != "" {
		t.Errorf("host on the instance's node = %q %v, want none", h, err)
	}

	src := netip.MustParsePrefix(edge.Subnet)
	w1, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	want := vip.Flow{Src: src, VIP: netip.MustParseAddr("10.208.1.2"), Port: 5432}
	if got := n.orch.linkFlows(ctx, n.ctl); len(got) != 1 || got[0] != want {
		t.Errorf("controller flows = %+v, want %+v", got, want)
	}
	want.Out = true
	if got := n.orch.vipBase(ctx, w1).Flows; !slices.Contains(got, want) {
		t.Errorf("w1 flows = %+v, want %+v", got, want)
	}
}
