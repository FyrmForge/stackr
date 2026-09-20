package service

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// NodeService owns a swarm node's life after it has joined: drain, activate,
// remove, and the group label tiles are placed by.
//
// The rules below lived only in the panel's servers screen, which is the
// product's only node surface today. That is exactly why they are here: an
// API for nodes is a natural next step, and every one of these is a rule a
// second surface would have to re-derive and would get wrong in a way nobody
// notices until a volume is unreachable.
type NodeService struct {
	store   repo.Store
	rt      *runtime.Runtime
	dataDir string
	// ensuring guards the retry loop. Boot starts one; so does every failing
	// Add node click, and during a registry outage that is one loop per click,
	// all pushing the same image.
	ensuring atomic.Bool
}

// NewNodeService creates a new node service.
func NewNodeService(store repo.Store, rt *runtime.Runtime, dataDir string) *NodeService {
	return &NodeService{store: store, rt: rt, dataDir: dataDir}
}

// PinnedTiles are the tiles holding a volume on this node's disk. Swarm never
// reschedules one — the node it is allowed to run on is this one — so they
// are the thing every node operation has to look at first.
func (s *NodeService) PinnedTiles(ctx context.Context, nodeID string) ([]repo.Tile, error) {
	tiles, err := s.store.ListTiles(ctx)
	if err != nil {
		return nil, err
	}
	var out []repo.Tile
	for i := range tiles {
		if tiles[i].HomeNode == nodeID {
			out = append(out, tiles[i])
		}
	}
	return out, nil
}

// Tasks splits a node's running tasks: the ones swarm reschedules by itself,
// and the pinned tiles, which it never does and must never be allowed to.
func (s *NodeService) Tasks(ctx context.Context, nodeID string) (stateless []runtime.NodeTaskInfo, pinned []repo.Tile, err error) {
	tasks, err := s.rt.NodeTasks(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	pinned, err = s.PinnedTiles(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	homed := map[string]bool{}
	for _, t := range pinned {
		homed[t.ID] = true
	}
	for _, t := range tasks {
		if t.TileID != "" && homed[t.TileID] {
			continue // already in the pinned list
		}
		stateless = append(stateless, t)
	}
	return stateless, pinned, nil
}

// Drain moves stateless tasks off and leaves the node in the swarm.
//
// It refuses a node a volume tile is pinned to. The panel renders that as a
// disabled button, which is a hint to a browser and nothing else: curl, a
// replayed form, or a page rendered before the tile was pinned all get
// through it, and the drain then takes that tile down with nowhere to
// reschedule it.
func (s *NodeService) Drain(ctx context.Context, sv *repo.Server) error {
	_, pinned, err := s.Tasks(ctx, sv.NodeID)
	if err != nil {
		return err
	}
	if len(pinned) > 0 {
		// Named. "Cannot drain" without saying what is in the way leaves the
		// operator to guess, and we already know.
		names := make([]string, len(pinned))
		for i, p := range pinned {
			names[i] = p.Name
		}
		return svcerr.Conflictf("these tiles hold volumes on %s and would have nowhere to run: %s. Move each one to another server first.",
			sv.Name, strings.Join(names, ", "))
	}
	return s.rt.SetNodeAvailability(ctx, sv.NodeID, "drain")
}

// Activate puts a drained node back into service.
func (s *NodeService) Activate(ctx context.Context, sv *repo.Server) error {
	return s.rt.SetNodeAvailability(ctx, sv.NodeID, "active")
}

// Remove drains the node and takes it out of the swarm.
//
// confirmed is the operator having typed the node's name back. Pinned tiles
// are a warning rather than a refusal here, unlike Drain: the volumes on that
// machine become unreachable either way once the node is gone, and typing the
// name is the acknowledgement of exactly that.
func (s *NodeService) Remove(ctx context.Context, sv *repo.Server, confirmed bool) error {
	if sv.Role == "manager" {
		return svcerr.Invalidf("", "the manager runs traefik, the registry and stackr itself; it cannot be removed")
	}
	if !confirmed {
		return svcerr.Invalidf("", "type %s to confirm: removing it leaves the volumes on that machine unreachable", sv.Name)
	}
	if sv.NodeID != "" {
		if err := s.rt.SetNodeAvailability(ctx, sv.NodeID, "drain"); err != nil {
			return err
		}
		if err := s.rt.RemoveNode(ctx, sv.NodeID); err != nil {
			return err
		}
	}
	// Before the row goes: a pending node removed because its join command
	// leaked must not still be joinable. The row is the only thing tying a key
	// to a node, so deleting it first leaves a live key pointing at nothing
	// the operator can see.
	if _, err := s.store.BurnServerJoinKeys(ctx, sv.ID); err != nil {
		return err
	}
	return s.store.DeleteServer(ctx, sv.ID)
}

// SetGroup writes the node's group label, which is what a tile's node_group is
// matched against. A node that has not joined has no swarm object to label.
func (s *NodeService) SetGroup(ctx context.Context, sv *repo.Server, group string) error {
	group = strings.TrimSpace(group)
	if sv.NodeID == "" {
		return svcerr.Invalidf("", "this node has not joined yet")
	}
	return s.rt.SetNodeGroup(ctx, sv.NodeID, group)
}

// agentRetries and agentBackoff are the one ensure policy. The case they
// exist for is the registry path: the agent image is pushed to the managed
// registry, which authenticates against this panel, which on the first
// attempt is either not listening yet (boot) or has just been asked to create
// a registry it has not finished creating (the first Add node).
const (
	agentRetries = 10
	agentBackoff = 15 * time.Second
)

// EnsureAgent creates the shared key and the global agent service.
//
// It tries once in the caller's own time and reports that attempt's error, so
// a caller with a page to render can say what is wrong on it. The retries
// then run in the background: boot had them and Add node did not, which meant
// the very first node to join a fresh install — the one case the retry was
// written for — got a single attempt and an agent that never came up.
func (s *NodeService) EnsureAgent(ctx context.Context) error {
	first := agent.Ensure(ctx, s.store, s.rt, s.dataDir)
	if first == nil {
		return nil
	}
	if !s.ensuring.CompareAndSwap(false, true) {
		// A loop is already retrying. The caller still gets this attempt's
		// error, which is what its page needs to say.
		return first
	}
	go func() {
		defer s.ensuring.Store(false)
		// Its own context: the request that started this is long gone by the
		// second attempt.
		bg := context.Background()
		for attempt := 2; attempt <= agentRetries; attempt++ {
			time.Sleep(agentBackoff)
			if err := agent.Ensure(bg, s.store, s.rt, s.dataDir); err == nil {
				slog.Info("node agent service started", "attempt", attempt)
				return
			} else if attempt == agentRetries {
				slog.Error("node agent service", "error", err)
				return
			} else {
				slog.Warn("node agent service, retrying", "attempt", attempt, "error", err)
			}
		}
	}()
	return first
}
