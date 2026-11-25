package service

import (
	"cmp"
	"context"
	"encoding/json"
	"path"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

// What the panel shows of a node's disk besides NodeDiskView. Reads only:
// sizes come from the cache, never from a helper run.

// DiskVolume is one volume on a node's disk, as the node drawer lists it.
type DiskVolume struct {
	ID, Slug  string
	Where     string // "shop / prod"
	Open      string // the env path its drawer opens under ("" = none)
	SizeBytes int64  // -1 = unknown
	Orphaned  bool   // its owner is gone; the retention clock runs
}

// NodeDiskPanel is the node drawer's Node disk section.
type NodeDiskPanel struct {
	NodeDiskView
	// Refusal is why a folder cannot be added on this node, "" = it can.
	Refusal      string
	Low          bool      // the disk is as full as the disk.full alert warns at
	MissingSince time.Time // zero = not known
	Volumes      []DiskVolume
	// InDocker counts the node's env volumes still in Docker storage, on a
	// ready disk; MoveLive is whether a volume-to-disk job holds the node.
	InDocker int
	MoveLive bool
}

// NodeDiskPanel reads the node's disk, its volumes (largest first) and, with
// no disk, whether a folder may be added here.
func (o *Orchestrator) NodeDiskPanel(ctx context.Context, nodeID string) (NodeDiskPanel, error) {
	v, err := o.NodeDisk(ctx, nodeID)
	if err != nil {
		return NodeDiskPanel{}, err
	}
	p := NodeDiskPanel{NodeDiskView: v}
	if !v.Has {
		// one reason for the whole Add disk button: only when neither a
		// folder nor a whole disk can be added here
		if err := o.folderDiskOK(ctx, nodeID); err != nil {
			n, nerr := o.store.Nodes.Get(ctx, nodeID)
			if nerr != nil {
				return p, nerr
			}
			if derr := o.deviceDiskOK(ctx, n); derr != nil {
				p.Refusal = derr.Error() // the whole-disk reason is the one to act on
			}
		}
		return p, nil
	}
	d, _, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return p, err
	}
	if v.State == disk.StateReady {
		if p.MoveLive, err = o.todiskLive(ctx, nodeID); err != nil {
			return p, err
		}
		if p.InDocker, err = o.inDockerCount(ctx, nodeID); err != nil {
			return p, err
		}
	}
	p.Low = v.Total > 0 && v.Free*100 <= v.Total*(100-diskFullPct)
	if v.State == disk.StateMissing {
		p.MissingSince = o.diskMissingSince(ctx, nodeID)
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return p, err
	}
	on := map[string]bool{}
	for _, f := range fs {
		on[f.Name] = true
	}
	live, err := o.store.Volumes.ListLive(ctx)
	if err != nil {
		return p, err
	}
	orph, err := o.store.Volumes.ListOrphaned(ctx)
	if err != nil {
		return p, err
	}
	live = append(live, orph...)
	live = slices.DeleteFunc(live, func(x Volume) bool { return !on[x.Name] })
	// Sizes only from the cache of a ready disk: no resolve, no Docker.
	var use map[string]hostdisk.Use
	if d.State == disk.StateReady {
		names := make([]string, len(live))
		for i, x := range live {
			names[i] = x.Name
		}
		use = o.cachedSizes(d, names)
	}
	for _, x := range live {
		r := DiskVolume{ID: x.ID, Slug: x.Slug, SizeBytes: -1, Where: x.ScopeKind, Orphaned: x.OrphanedAt != nil}
		if u, ok := use[x.Name]; ok && !u.Missing {
			r.SizeBytes = u.Bytes
		}
		if x.ScopeKind == ScopeEnv {
			if e, err := o.envs.Get(ctx, x.ScopeID); err == nil {
				r.Where = e.Name
				if st, err := o.stacks.Get(ctx, e.StackID); err == nil {
					r.Where = st.Name + " / " + e.Name
				}
				r.Open = "/" + o.scopePath(ctx, ScopeEnv, e.ID)
			}
		}
		p.Volumes = append(p.Volumes, r)
	}
	slices.SortFunc(p.Volumes, func(a, b DiskVolume) int {
		return cmp.Or(cmp.Compare(b.SizeBytes, a.SizeBytes), cmp.Compare(a.Slug, b.Slug))
	})
	return p, nil
}

// diskMissingSince is when the disk's missing problem opened; zero if unknown.
func (o *Orchestrator) diskMissingSince(ctx context.Context, nodeID string) time.Time {
	s, ok, err := o.store.Settings.Get(ctx, problemKey(diskMissingKey(nodeID)))
	if err != nil || !ok {
		return time.Time{}
	}
	var r problemRow
	if json.Unmarshal([]byte(s), &r) != nil {
		return time.Time{}
	}
	return r.Since
}

// VolumeStorage is where one volume lives, for its drawer.
type VolumeStorage struct {
	OnDisk, InDocker bool
	State            string // the disk's: adding | ready | missing
	Folder           string // the folder on the node ("" = none)
	Node, NodeID     string
	SizeBytes        int64     // -1 = unknown
	SizeAt           time.Time // when the disk's sizes were read; zero = not known
	// Ghost is what the last gate decision found beside the folder: "copy" (a
	// plain Docker copy), "bound" (an object bound to Device, not the folder)
	// or "". Memory only, no Docker; Folder is the folder it should be.
	Ghost, Device string
}

// VolumeStorage reads where a volume lives. It never places or measures
// anything, and a size is the disk's cached one.
func (o *Orchestrator) VolumeStorage(ctx context.Context, volumeID string) (VolumeStorage, error) {
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil {
		return VolumeStorage{}, err
	}
	rt, nodeID, err := o.volumes.Where(ctx, v)
	if err != nil {
		return VolumeStorage{}, err
	}
	// -1: a poll never asks Docker for a size; a disk volume's is set below.
	s := VolumeStorage{NodeID: nodeID, InDocker: true, SizeBytes: -1}
	if n, err := o.store.Nodes.Get(ctx, nodeID); err == nil {
		s.Node = n.Name
	}
	// a bound object with no folder row is routed legacy but still a ghost
	dev, seen := o.volumes.GhostSeen(nodeID, v.Name)
	if rt == volume.RouteLegacy && !seen {
		return s, nil
	}
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return s, err
	}
	s.OnDisk, s.State = true, d.State
	s.InDocker = false
	s.Folder = path.Join(d.MountPath, d.ID, v.Name)
	if seen && d.State == disk.StateReady {
		s.Ghost, s.Device, s.InDocker = "bound", dev, false
		if dev == "" {
			s.Ghost, s.InDocker = "copy", true
		}
	}
	if d.State == disk.StateReady {
		if u, ok := o.cachedSizes(d, []string{v.Name})[v.Name]; ok && !u.Missing {
			s.SizeBytes = u.Bytes
		}
		s.SizeAt = o.diskSizesAt(nodeID)
	}
	return s, nil
}

// diskSizesAt is when the node's disk sizes were last read.
func (o *Orchestrator) diskSizesAt(nodeID string) time.Time {
	c := &o.diskSize
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.by[nodeID]; e != nil {
		return e.at
	}
	return time.Time{}
}

// VolumeOnDisk is whether the volume has a folder on its node's disk, from
// the rows alone (no Docker, no helper).
func (o *Orchestrator) VolumeOnDisk(ctx context.Context, v Volume) bool {
	return o.volumes.OnDisk(ctx, v)
}

// cutOffMove is a volume whose move copied it and then failed before the
// deploy: the row names the target and a tile's newest job is the failed move.
// Job rows only, so a poll may ask; halfMoved asks Docker and settles it.
func (o *Orchestrator) cutOffMove(ctx context.Context, v Volume) (bool, error) {
	if !isMoveName(v.ID, v.Name) {
		return false, nil
	}
	ts, err := o.moveTiles(ctx, []Volume{v})
	if err != nil {
		return false, err
	}
	for _, t := range ts {
		j, ok, err := o.jobRows.Last(ctx, t.ID)
		if err != nil {
			return false, err
		}
		if ok && j.Kind == string(kindVolumeToDisk) && j.State == string(job.Failed) {
			return true, nil
		}
	}
	return false, nil
}

// inDockerCount is how many live env volumes of the node have no folder on
// its disk, or a move cut off after the copy. Rows only.
func (o *Orchestrator) inDockerCount(ctx context.Context, nodeID string) (int, error) {
	live, err := o.store.Volumes.ListLive(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range live {
		if v.ScopeKind != ScopeEnv || o.volumeNode(ctx, v.ID) != nodeID {
			continue
		}
		if !o.volumes.OnDisk(ctx, v) {
			n++
		} else if cut, err := o.cutOffMove(ctx, v); err != nil {
			return 0, err
		} else if cut {
			n++
		}
	}
	return n, nil
}
