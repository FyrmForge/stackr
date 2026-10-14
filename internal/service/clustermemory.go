package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
)

// nodeMemory is what the map last saw of a node while it answered, kept so
// an offline node stays in its box with its tunnels drawn down instead of
// vanishing. A settings row per node (no schema change); forget drops it.
type nodeMemory struct {
	Net    string            `json:"net,omitempty"`
	Public []string          `json:"public,omitempty"`
	Docker string            `json:"docker,omitempty"`
	Arch   string            `json:"arch,omitempty"`
	Links  map[string]string `json:"links,omitempty"` // peer id -> link kind: direct tunnels only
	Via    []string          `json:"via,omitempty"`   // node ids its routed peers reach it through
}

func memoryKey(id string) string { return "cluster.node." + id }

func (o *Orchestrator) loadMemory(ctx context.Context, id string) *nodeMemory {
	v, ok, err := o.store.Settings.Get(ctx, memoryKey(id))
	if err != nil || !ok || v == "" {
		return nil
	}
	var m nodeMemory
	if json.Unmarshal([]byte(v), &m) != nil {
		return nil
	}
	return &m
}

// remember updates the memory of every node that answered: its box,
// addresses and routes as seen now; a tunnel stays remembered while its peer
// is away and is dropped only once both ends are up and the pair is routed.
func (o *Orchestrator) remember(ctx context.Context, m *ClusterMap, in []clusterNodeIn, direct func(a, b string) (bool, bool)) {
	ids := map[string]string{}
	online := map[string]bool{}
	for _, n := range m.Nodes {
		ids[n.Name] = n.ID
		online[n.ID] = n.Status == NodeOnline
	}
	for i, n := range m.Nodes {
		if !in[i].Up || n.Net == "unknown" {
			continue
		}
		old := in[i].Mem
		mem := nodeMemory{Net: n.Net, Public: n.Public, Docker: n.Docker, Arch: n.Arch, Links: map[string]string{}}
		if old != nil {
			maps.Copy(mem.Links, old.Links)
		}
		for _, l := range m.Links {
			if l.Down || (l.A != n.ID && l.B != n.ID) {
				continue
			}
			peer := l.A
			if peer == n.ID {
				peer = l.B
			}
			if d, ok := direct(l.A, l.B); ok && d {
				mem.Links[peer] = l.Kind
			}
		}
		for peer := range mem.Links {
			if d, ok := direct(n.ID, peer); online[peer] && ok && !d {
				delete(mem.Links, peer) // now routed
			}
		}
		for _, name := range n.ReachedVia {
			if id, ok := ids[name]; ok {
				mem.Via = append(mem.Via, id)
			}
		}
		b, _ := json.Marshal(mem)
		if old != nil {
			if ob, _ := json.Marshal(old); string(ob) == string(b) {
				continue
			}
		}
		if err := o.store.Settings.Set(ctx, memoryKey(n.ID), string(b)); err != nil {
			slog.Warn("cluster: remember node", "node", n.Name, "err", err)
		}
	}
}

// recall fills what an offline node no longer reports: the hops it was
// reached through, each marked down when that hop is not online either.
func recall(m *ClusterMap, in []clusterNodeIn) {
	names := map[string]string{}
	online := map[string]bool{}
	for _, n := range m.Nodes {
		names[n.ID] = n.Name
		online[n.ID] = n.Status == NodeOnline
	}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		if mem := in[i].Mem; n.Status != NodeOnline && mem != nil && len(n.ReachedVia) == 0 {
			for _, id := range mem.Via {
				if name, ok := names[id]; ok {
					n.ReachedVia = append(n.ReachedVia, name)
				}
			}
		}
		for _, name := range n.ReachedVia {
			for id, nm := range names {
				if nm == name && !online[id] && !slices.Contains(n.ViaDown, name) {
					n.ViaDown = append(n.ViaDown, name)
				}
			}
		}
	}
}
