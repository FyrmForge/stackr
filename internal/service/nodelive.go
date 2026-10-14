package service

import (
	"context"
	"net/netip"
	"runtime"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// controllerFacts is what the controller node reports about itself. On a
// multi-node install the mesh already reads it through localMesh, the same
// code a worker runs; a single node or a fake host has no mesh, so the
// facts are built from the interfaces and the daemon it talks to.
func (o *Orchestrator) controllerFacts(ctx context.Context, n store.Node) *worker.Facts {
	if o.mesh != nil {
		if f, ok := o.mesh.factsOf(n.ID); ok {
			return &f
		}
	}
	f := worker.Facts{Interfaces: worker.Interfaces(), Arch: runtime.GOARCH, Version: o.cfg.Version}
	if h, err := o.docker.HostInfo(docker.WithNode(ctx, n.ID)); err == nil {
		f.CPUs, f.MemBytes, f.Docker = h.CPUs, uint64(h.MemBytes), h.Version
	}
	f.DiskFree, f.DiskTotal = o.dataDirDisk()
	return &f
}

// NodeLive is what the node drawer shows live: the node's facts, when the
// controller last heard from it and its addresses grouped by kind.
type NodeLive struct {
	Facts    *worker.Facts // controller via controllerFacts, worker via reportedFacts
	LastSeen time.Time     // when the facts last came in; the controller: now
	Ifaces   []NodeIface
}

// NodeIface is one address of a node. Group is public, lan, cluster, docker
// or hidden (loopback, link-local, veth: kept out of the way).
type NodeIface struct{ Group, Name, Addr, Label string }

const (
	IfacePublic  = "public"
	IfaceLAN     = "lan"
	IfaceCluster = "cluster"
	IfaceDocker  = "docker"
	IfaceHidden  = "hidden"
)

// NodeLive reads a node's facts, last-seen time and grouped interfaces.
func (o *Orchestrator) NodeLive(ctx context.Context, id string) (NodeLive, error) {
	n, err := o.store.Nodes.Get(ctx, id)
	if err != nil {
		return NodeLive{}, err
	}
	out := NodeLive{Facts: o.reportedFacts(ctx, n)}
	if n.Role == node.Controller {
		out.LastSeen = time.Now()
	} else if o.mesh != nil {
		out.LastSeen = o.mesh.seenAt(n.ID)
	}
	if out.Facts == nil {
		return out, nil
	}
	envs := map[string]string{}
	es, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return out, err
	}
	for _, e := range es {
		if p, err := netip.ParsePrefix(e.Subnet); err == nil {
			if ne, err := o.nodeEnvOut(ctx, e); err == nil {
				envs[p.Masked().String()] = ne.Path
			}
		}
	}
	out.Ifaces = groupIfaces(*out.Facts, envs)
	return out, nil
}

// seenAt is when a node's facts last came in (zero: never).
func (m *meshSvc) seenAt(id string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.at[id]
}

// groupIfaces sorts every address of a node into a group, per address: a
// public NIC's fe80 is link-local, not public. envs maps an env subnet to
// its org/stack/env path.
// ponytail: bridges named by subnet, not by Docker network; a job's network shows as "stackr network".
func groupIfaces(f worker.Facts, envs map[string]string) []NodeIface {
	var out []NodeIface
	for _, i := range f.Interfaces {
		for _, s := range i.Prefixes {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				continue
			}
			a := p.Addr().Unmap()
			g, label := IfaceLAN, ""
			switch {
			case i.Name == "lo" || a.IsLoopback():
				g, label = IfaceHidden, "loopback"
			case a.IsLinkLocalUnicast():
				g, label = IfaceHidden, "link-local"
			case strings.HasPrefix(i.Name, "veth"):
				g, label = IfaceHidden, "veth"
			case i.Name == mesh.Iface:
				g, label = IfaceCluster, "tunnel"
			case strings.HasPrefix(i.Name, "br-"):
				if path, ok := envs[p.Masked().String()]; ok {
					g, label = IfaceCluster, path
				} else if mesh.Supernet.Contains(a) {
					g, label = IfaceCluster, "stackr network"
				} else {
					g = IfaceDocker
				}
			case i.Name == "docker0":
				g = IfaceDocker
			case a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a):
				g = IfacePublic
			}
			out = append(out, NodeIface{Group: g, Name: i.Name, Addr: s, Label: label})
		}
	}
	return out
}
