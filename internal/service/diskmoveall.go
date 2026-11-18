package service

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Move all to disk: plans, groups and enqueues volume-to-disk jobs (see
// volumemove.go). It has no job and no loop of its own: one job per group,
// smallest first, serialised by the node's todisk key.

// MoveAllVolume is one volume of the plan.
type MoveAllVolume struct {
	ID        string
	Slug      string
	Env       string // "stack / env"
	SizeBytes int64  // -1 = cannot tell
	Finishes  bool   // an earlier move swapped the row; only the deploy is left
	Reason    string // set on a skipped volume
}

// MoveAllGroup is one job: a connected component of volumes and the tiles
// that mount them. Its tiles stop once, for the group's copy.
type MoveAllGroup struct {
	Volumes   []MoveAllVolume
	Tiles     []string // names of the tiles that restart
	SizeBytes int64    // bytes to copy (a finishing volume copies nothing)
}

// MoveAllPlan is what the Move all dialog shows before the confirm.
type MoveAllPlan struct {
	NodeID  string
	Groups  []MoveAllGroup  // smallest first, the enqueue order
	Skipped []MoveAllVolume // each with its Reason
	Need    int64           // bytes to copy plus a tenth
	Free    uint64          // free bytes on the node's disk
	Fits    bool            // Need <= Free
	Busy    bool            // a volume-to-disk job is live on this node: the verb is refused
}

// PlanMoveAllToDisk is read only (route verb node.manage). It is advisory:
// each group job judges its group again when it runs.
func (o *Orchestrator) PlanMoveAllToDisk(ctx context.Context, by *Principal, nodeID string) (MoveAllPlan, error) {
	p := MoveAllPlan{NodeID: nodeID}
	if _, err := o.diskNodeUp(ctx, nodeID); err != nil {
		return p, err
	}
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return p, err
	}
	if !has || d.State != disk.StateReady {
		return p, errs.Refusedf("This node's disk is not ready.")
	}
	if p.Free, _, err = o.dataDiskOf(ctx, nodeID); err != nil {
		return p, err
	}
	if p.Busy, err = o.todiskLive(ctx, nodeID); err != nil {
		return p, err
	}
	live, err := o.store.Volumes.ListLive(ctx)
	if err != nil {
		return p, err
	}
	live = slices.DeleteFunc(live, func(v Volume) bool {
		return v.ScopeKind != "env" || o.volumeNode(ctx, v.ID) != nodeID
	})
	sizes := o.volumes.SizesFresh(ctx, live)

	var movable []Volume
	finishes := map[string]bool{}
	for _, v := range live {
		half, err := o.halfMoved(ctx, v)
		if err != nil {
			return p, err
		}
		if !half && o.volumes.OnDisk(ctx, v) {
			continue // already on the disk
		}
		if half {
			finishes[v.ID] = true
		} else if reason, err := o.moveSkip(ctx, v, sizes[v.ID], p.Free); err != nil {
			return p, err
		} else if reason != "" {
			p.Skipped = append(p.Skipped, MoveAllVolume{ID: v.ID, Slug: v.Slug, Env: o.moveEnvName(ctx, v), SizeBytes: sizes[v.ID], Reason: reason})
			continue
		}
		movable = append(movable, v)
	}
	if p.Groups, err = o.moveGroups(ctx, movable, finishes, sizes); err != nil {
		return p, err
	}
	var sum int64
	for _, g := range p.Groups {
		sum += g.SizeBytes
	}
	p.Need = sum + sum/10
	p.Fits = uint64(p.Need) <= p.Free
	return p, nil
}

// MoveAllToDisk enqueues one volume-to-disk job per group (route verb
// node.manage), smallest first. A second press is refused while any
// volume-to-disk job holds the node's todisk key. A group that cannot be
// queued is refused on its own: the others are still queued, and the jobs
// that were queued come back with the joined refusals.
func (o *Orchestrator) MoveAllToDisk(ctx context.Context, by *Principal, nodeID string) ([]Job, error) {
	// The Busy check and the enqueues are one step: two presses at once would
	// both see Busy=false and split the groups. A press that loses is refused
	// as Busy.
	// ponytail: in-process, so one controller process is the ceiling; a second
	// controller needs the guard in the job store (an exclusive enqueue).
	mu, _ := o.moveAllLocks.LoadOrStore(nodeID, &sync.Mutex{})
	if !mu.(*sync.Mutex).TryLock() {
		return nil, errs.Conflictf("Volumes are already moving to the disk.")
	}
	defer mu.(*sync.Mutex).Unlock()
	p, err := o.PlanMoveAllToDisk(ctx, by, nodeID)
	if err != nil {
		return nil, err
	}
	if p.Busy {
		return nil, errs.Conflictf("Volumes are already moving to the disk.")
	}
	if !p.Fits {
		return nil, errs.Conflictf("The disk has %s free and the moves need %s (the data and a tenth over).", gb(p.Free), gb(uint64(p.Need)))
	}
	if len(p.Groups) == 0 {
		return nil, errs.Refusedf("Nothing to move.")
	}
	var js []Job
	var failed []error
groups:
	for _, g := range p.Groups {
		vs := make([]Volume, 0, len(g.Volumes))
		for _, mv := range g.Volumes {
			v, err := o.volumes.Get(ctx, mv.ID)
			if err != nil {
				failed = append(failed, err)
				continue groups
			}
			vs = append(vs, v)
		}
		j, err := o.enqueueVolumeMove(ctx, by, nodeID, vs)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		js = append(js, j)
	}
	return js, errors.Join(failed...)
}

// todiskLive is whether a live job holds the node's todisk key.
func (o *Orchestrator) todiskLive(ctx context.Context, nodeID string) (bool, error) {
	live, err := o.jobRows.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(live, func(j store.Job) bool { return slices.Contains(j.LockSet, todiskKey(nodeID)) }), nil
}

func (o *Orchestrator) moveEnvName(ctx context.Context, v Volume) string {
	e, err := o.envs.Get(ctx, v.ScopeID)
	if err != nil {
		return ""
	}
	if st, err := o.stacks.Get(ctx, e.StackID); err == nil {
		return st.Name + " / " + e.Name
	}
	return e.Name
}

// halfMoved is a volume an earlier move left with only its deploy to do: the
// row names the target, the disk has its folder row, and a tile that should be
// up has no container.
func (o *Orchestrator) halfMoved(ctx context.Context, v Volume) (bool, error) {
	if !isMoveName(v.ID, v.Name) || !o.volumes.OnDisk(ctx, v) {
		return false, nil
	}
	ts, err := o.moveTiles(ctx, []Volume{v})
	if err != nil {
		return false, err
	}
	for _, t := range ts {
		if t.Paused || t.Kind == tile.Slice || tile.RunToCompletion(t.Kind) {
			continue
		}
		stopped, err := o.stoppedByUser(ctx, t)
		if err != nil {
			return false, err
		}
		if stopped {
			continue
		}
		tctx, err := o.withTileNode(ctx, t.ID)
		if err != nil {
			return false, err
		}
		if cs, err := o.tiles.Replicas(tctx, t); err == nil && len(cs) == 0 {
			return true, nil
		}
	}
	return false, nil
}

// moveSkip is why a volume is not moved by Move all, "" when it is.
func (o *Orchestrator) moveSkip(ctx context.Context, v Volume, size int64, free uint64) (string, error) {
	if v.InstanceID != nil {
		if err := o.noStandbys(ctx, *v.InstanceID); err != nil {
			if errors.As(err, new(errs.Conflict)) {
				return "Remove the standbys first.", nil
			}
			return "", err
		}
		if err := o.notRelocated(ctx, *v.InstanceID); err != nil {
			if errors.As(err, new(errs.Refused)) {
				return "Its primary runs on another node after a failover.", nil
			}
			return "", err
		}
	}
	if o.volumeIdle(ctx, v) != nil {
		return "A backup or another job holds it.", nil
	}
	if size > 0 && free < uint64(size)+uint64(size)/10 {
		return "It does not fit on the disk.", nil
	}
	return "", nil
}

// moveGroups are the connected components of vs, volumes linked through the
// tiles that mount them (A-V1-B-V2-C is one group), smallest first.
func (o *Orchestrator) moveGroups(ctx context.Context, vs []Volume, finishes map[string]bool, sizes map[string]int64) ([]MoveAllGroup, error) {
	parent := make([]int, len(vs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	tilesOf := make([][]Tile, len(vs))
	owner := map[string]int{} // tile id -> first volume that mounts it
	for i, v := range vs {
		ts, err := o.moveTiles(ctx, []Volume{v})
		if err != nil {
			return nil, err
		}
		tilesOf[i] = ts
		for _, t := range ts {
			if j, ok := owner[t.ID]; ok {
				parent[find(i)] = find(j)
			} else {
				owner[t.ID] = i
			}
		}
	}
	byRoot := map[int]*MoveAllGroup{}
	var roots []int
	tileSeen := map[string]bool{}
	for i, v := range vs {
		r := find(i)
		g := byRoot[r]
		if g == nil {
			g = &MoveAllGroup{}
			byRoot[r] = g
			roots = append(roots, r)
		}
		g.Volumes = append(g.Volumes, MoveAllVolume{
			ID: v.ID, Slug: v.Slug, Env: o.moveEnvName(ctx, v), SizeBytes: sizes[v.ID], Finishes: finishes[v.ID],
		})
		if !finishes[v.ID] {
			g.SizeBytes += max(sizes[v.ID], 0)
		}
		for _, t := range tilesOf[i] {
			if !tileSeen[t.ID] {
				tileSeen[t.ID] = true
				g.Tiles = append(g.Tiles, t.Name)
			}
		}
	}
	out := make([]MoveAllGroup, 0, len(roots))
	for _, r := range roots {
		out = append(out, *byRoot[r])
	}
	slices.SortStableFunc(out, func(a, b MoveAllGroup) int {
		return cmp.Or(cmp.Compare(a.SizeBytes, b.SizeBytes), cmp.Compare(a.Volumes[0].Slug, b.Volumes[0].Slug))
	})
	return out, nil
}
