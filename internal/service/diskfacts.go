package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// What the controller knows of a node's disk without asking the node's
// filesystem: the worker's facts (free and total, from its own background
// refresh), a size cache fed by a background walk, and the registry of disk
// paths it pushes to the worker. A poll never runs a helper.

// sizeTTL is how old a cached usage may be before a read starts a refresh.
const sizeTTL = time.Minute

type sizeEntry struct {
	at   time.Time
	use  map[string]hostdisk.Use
	busy bool
}

// diskSizes is the usage cache, one entry per node (one disk each).
type diskSizes struct {
	mu sync.Mutex
	by map[string]*sizeEntry
}

// cachedSizes answers from the last walk and, when it is old or never ran,
// starts one (one per node at a time). A name it has no answer for is left
// out: the caller reads that as unknown.
func (o *Orchestrator) cachedSizes(d store.NodeDisk, names []string) map[string]hostdisk.Use {
	c := &o.diskSize
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[string]*sizeEntry{}
	}
	e := c.by[d.NodeID]
	if e == nil {
		e = &sizeEntry{}
		c.by[d.NodeID] = e
	}
	if !e.busy && time.Since(e.at) > sizeTTL {
		e.busy = true
		o.track(func() { o.walkSizes(d, e) })
	}
	out := map[string]hostdisk.Use{}
	for _, n := range names {
		if u, ok := e.use[n]; ok {
			out[n] = u
		}
	}
	return out
}

// walkSizes reads every folder's size on the disk and keeps the answer. A
// failed walk keeps the last numbers and tries again after the TTL.
func (o *Orchestrator) walkSizes(d store.NodeDisk, e *sizeEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), o.dh.long)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-o.stopNodes:
			cancel()
		case <-done:
		}
	}()
	var use map[string]hostdisk.Use
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err == nil {
		names := make([]string, len(fs))
		for i, f := range fs {
			names[i] = f.Name
		}
		if len(names) > 0 {
			use, err = o.diskLeaf.Usage(ctx, d, true, names...)
		}
	}
	o.diskSize.mu.Lock()
	defer o.diskSize.mu.Unlock()
	e.busy, e.at = false, time.Now()
	if err == nil {
		e.use = use
	}
}

// diskWorker reports a node whose worker must say it understands disks: any
// worker, and a controller that runs its own. A controller with no worker
// (a fake host) has none to be too old.
func (o *Orchestrator) diskWorker(n store.Node) bool {
	return n.Role != node.Controller || o.localWorker != nil
}

// diskFacts is the node's last facts for its disk, nil when it has none. The
// controller's own worker answers directly, even on a one-node install where
// the mesh does not read it.
func (o *Orchestrator) diskFacts(ctx context.Context, n store.Node) *worker.Facts {
	if n.Role == node.Controller && o.localWorker != nil {
		return o.controllerFacts(ctx, n) // short cache, no Docker or pings per poll
	}
	return o.reportedFacts(ctx, n)
}

// dataDiskOf is the free and total bytes of the disk a node's data lives on.
// A node with no disk row has the Docker disk, as before. A node with a disk
// row has only that disk: not ready is *disk.MissingError, an unread disk is
// an error, and a failed row lookup is that error. It never falls back to the
// Docker disk for a node that has a row.
func (o *Orchestrator) dataDiskOf(ctx context.Context, nodeID string) (free, total uint64, err error) {
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return 0, 0, err
	}
	if !has {
		free, total = o.diskOf(ctx, nodeID)
		return free, total, nil
	}
	if d.State != disk.StateReady {
		return 0, 0, &disk.MissingError{Node: nodeID}
	}
	n, err := o.store.Nodes.Get(ctx, nodeID)
	if err != nil {
		return 0, 0, err
	}
	if f := o.diskFacts(ctx, n); f != nil {
		for _, x := range f.Disks {
			if x.Path == d.MountPath && x.Total > 0 {
				return x.Free, x.Total, nil
			}
		}
	}
	return 0, 0, errs.Refusedf("The node has not read its disk yet.")
}

// diskRegs are the disk paths a node's worker is to report: one per row.
func (o *Orchestrator) diskRegs(ctx context.Context, nodeID string) ([]worker.DiskReg, error) {
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return nil, err
	}
	return []worker.DiskReg{{ID: d.ID, Path: d.MountPath, Owner: d.NodeID}}, nil
}

// diskNode is the way to a node's worker for a registry push.
func (o *Orchestrator) diskNode(n store.Node) (MeshNode, bool) {
	if o.mesh != nil {
		if mn, ok := o.mesh.nodes(n.ID); ok {
			return mn, true
		}
	}
	if n.Role == node.Controller {
		if o.localWorker != nil {
			return localMesh{o.localWorker}, true
		}
		return nil, false
	}
	c, err := o.nodeExtras(n.ID)
	return c, err == nil
}

// pushDisks replaces the node worker's registry with the node's rows.
func (o *Orchestrator) pushDisks(ctx context.Context, n store.Node) error {
	want, err := o.diskRegs(ctx, n.ID)
	if err != nil {
		return err
	}
	if !o.diskWorker(n) {
		return nil
	}
	find := o.diskNodeOf
	if find == nil {
		find = o.diskNode
	}
	mn, ok := find(n)
	if !ok {
		return fmt.Errorf("no way to reach %s", n.Name)
	}
	if want == nil {
		want = []worker.DiskReg{}
	}
	return mn.PutDisks(ctx, want)
}

// reconcileDisks is called with the facts a poll just read for n. When the
// registry the worker echoes differs from the node's rows it pushes the rows,
// so a worker that restarted empty, or missed a push, is put right on the next
// poll and not only after an offline-to-online change. A worker that does not
// report DisksRead is too old to push to and is left alone. It never runs a
// helper and never changes a row.
func (o *Orchestrator) reconcileDisks(ctx context.Context, n store.Node, f worker.Facts) error {
	if !f.DisksRead {
		return nil
	}
	want, err := o.diskRegs(ctx, n.ID)
	if err != nil {
		return err
	}
	if sameRegs(want, f.DiskRegs) {
		return nil
	}
	return o.pushDisks(ctx, n)
}

func sameRegs(a, b []worker.DiskReg) bool {
	key := func(r worker.DiskReg) string { return r.ID + "\x00" + r.Path + "\x00" + r.Owner }
	ka, kb := make([]string, len(a)), make([]string, len(b))
	for i, r := range a {
		ka[i] = key(r)
	}
	for i, r := range b {
		kb[i] = key(r)
	}
	slices.Sort(ka)
	slices.Sort(kb)
	return strings.Join(ka, "\n") == strings.Join(kb, "\n")
}
