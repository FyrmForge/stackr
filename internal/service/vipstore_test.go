package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// The minute rebuild costs one container list per node, whatever the tile
// count: no per-tile list, no inspect.
func TestRebuildOneListPerNode(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	for i := range 5 {
		tl, err := n.orch.tiles.Create(ctx, Tile{StackID: n.stack, EnvironmentID: n.dev.ID, Name: fmt.Sprintf("t%d", i), Kind: "image", ImageRef: "nginx"})
		must(t, err)
		n.ctlFake.Containers = append(n.ctlFake.Containers,
			docker.Container{ID: "p" + tl.ID, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "pause"},
				Nets: map[string]string{n.dev.Network: fmt.Sprintf("10.213.1.%d", 2+i)}},
			docker.Container{ID: "r" + tl.ID, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
				Nets: map[string]string{n.dev.Network: fmt.Sprintf("10.213.1.%d", 130+i)}})
	}
	must(t, n.orch.rebuildVIPs(ctx))
	lists, other := 0, 0
	for _, c := range n.ctlFake.Calls() {
		switch c.Method {
		case "List":
			lists++
		case "Inspect":
			if c.Args[0] != "stackr-proxy" { // the controller's Caddy address: once per rebuild, not per tile
				other++
			}
		}
	}
	if lists != 1 || other != 0 {
		t.Fatalf("controller: %d lists, %d inspects; want 1 and 0: %v", lists, other, n.ctlFake.Calls())
	}
	if len(*n.ctlRS) != 1 {
		t.Fatalf("restores = %d", len(*n.ctlRS))
	}
}

// A stored VIP that no longer matches the pause container is drift: the
// rebuild stores the real one.
func TestRebuildReconcilesStoredVIP(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	must(t, n.st.TileVIPs.Set(ctx, n.devT.ID, n.ctl.ID, "10.213.1.9"))
	n.ctlFake.Containers = append(n.ctlFake.Containers,
		docker.Container{ID: "p", State: "running", Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "pause"},
			Nets: map[string]string{n.dev.Network: "10.213.1.2"}})
	must(t, n.orch.rebuildVIPs(ctx))
	if v, _ := n.st.TileVIPs.Get(ctx, n.devT.ID, n.ctl.ID); v != "10.213.1.2" {
		t.Fatalf("stored VIP = %q, want the real 10.213.1.2", v)
	}
	if v, ok := n.orch.tileVIP(ctx, n.ctl.ID, n.devT.ID); !ok || v.String() != "10.213.1.2" {
		t.Fatalf("tileVIP = %v %v", v, ok)
	}
}

// A pause container is pinned: the first one gets a stable pick in the env
// network's lower half, and once that is stored a recreated pause (a restart
// that lost it, a redeploy) asks for the same address again.
func TestPauseIsPinned(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.213.1.0/24"))
	_, err := n.orch.tiles.Pause(ctx, n.devT, n.dev.Network)
	_ = err // the fake's run hands back no address; only the spec matters here
	if len(n.ctlFake.Specs) == 0 || n.ctlFake.Specs[0].Networks[0].IPv4 == "" {
		t.Fatalf("first pause not pinned: %+v", n.ctlFake.Specs)
	}
	first := n.ctlFake.Specs[0].Networks[0].IPv4
	must(t, n.st.TileVIPs.Set(ctx, n.devT.ID, n.ctl.ID, first))
	n.ctlFake.Specs = nil
	_, _ = n.orch.tiles.Pause(ctx, n.devT, n.dev.Network)
	if len(n.ctlFake.Specs) == 0 || n.ctlFake.Specs[0].Networks[0].IPv4 != first {
		t.Fatalf("recreated pause asked for %+v, want %s", n.ctlFake.Specs, first)
	}
}
