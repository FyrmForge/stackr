package service

import (
	"context"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/worker"
)

// TunnelEnd is one side of a node pair's tunnel, as that node sees it.
type TunnelEnd struct {
	Dials     bool      // has an endpoint for the other side
	Endpoint  string    // Candidates[0], "" = waits to be dialled
	SeenFrom  string    // Facts.Seen[peer key]: where the other side's packets arrive from
	Handshake time.Time // Facts.Handshake, zero = not reported
	Rx, Tx    int64     // Facts.Rx/Tx[peer key], -1 = not reported
}

// fillEnds sets Ends on every link: [0] is A's side of the pair, [1] B's.
// The mesh decides who dials: a node whose spec gives the peer endpoint
// candidates dials them, one without waits. Nothing new comes from workers.
func fillEnds(m *ClusterMap, in []clusterNodeIn, spec func(string) (worker.MeshSpec, bool)) {
	facts := map[string]*worker.Facts{}
	for _, n := range in {
		facts[n.Node.ID] = n.Facts
	}
	end := func(self, peer string) TunnelEnd {
		e := TunnelEnd{Rx: -1, Tx: -1}
		sp, ok := spec(self)
		if !ok {
			return e
		}
		i := slices.IndexFunc(sp.Peers, func(p mesh.Peer) bool { return p.NodeID == peer })
		if i < 0 {
			return e
		}
		p := sp.Peers[i]
		if len(p.Candidates) > 0 {
			e.Dials, e.Endpoint = true, p.Candidates[0].String()
		}
		f := facts[self]
		if f == nil || p.PublicKey == "" {
			return e
		}
		e.SeenFrom = f.Seen[p.PublicKey]
		if s := f.Handshake[p.PublicKey]; s > 0 {
			e.Handshake = time.Unix(s, 0)
		}
		if v, ok := f.Rx[p.PublicKey]; ok {
			e.Rx = v
		}
		if v, ok := f.Tx[p.PublicKey]; ok {
			e.Tx = v
		}
		return e
	}
	for _, ls := range [][]ClusterLink{m.Links, m.Pairs} {
		for i := range ls {
			ls[i].Ends = [2]TunnelEnd{end(ls[i].A, ls[i].B), end(ls[i].B, ls[i].A)}
		}
	}
}

// NodeTunnel is one tunnel of a node: the peer and the pair's link.
type NodeTunnel struct {
	Peer, PeerName string
	Link           ClusterLink
}

// NodeTunnels are the node's tunnels, direct and routed, for its drawer. The
// drawer links each row to the tunnel drawer with cluster.TunnelDrawer.
// ponytail: builds the whole map; cached facts make it cheap.
func (o *Orchestrator) NodeTunnels(ctx context.Context, id string) ([]NodeTunnel, error) {
	m, err := o.ClusterMap(ctx)
	if err != nil {
		return nil, err
	}
	return tunnelsOf(m, id), nil
}

// tunnelsOf is the node's tunnels in the map's pairs, else its links.
func tunnelsOf(m ClusterMap, id string) []NodeTunnel {
	names := map[string]string{}
	for _, n := range m.Nodes {
		names[n.ID] = n.Name
	}
	pairs := m.Pairs
	if len(pairs) == 0 {
		pairs = m.Links // no mesh: no routed pairs to add
	}
	var out []NodeTunnel
	for _, l := range pairs {
		peer := l.A
		switch id {
		case l.A:
			peer = l.B
		case l.B:
		default:
			continue
		}
		out = append(out, NodeTunnel{Peer: peer, PeerName: names[peer], Link: l})
	}
	return out
}
