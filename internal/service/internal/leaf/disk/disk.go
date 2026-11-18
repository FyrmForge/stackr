// Package disk owns node_disks and disk_folders: the one disk a node's
// volumes may live on, its folder rows, and the filesystem they stand for.
// The filesystem work is a hostdisk.Request run on the node through Host.
// "No disk" is no row; any row that is not ready refuses with *MissingError.
package disk

import (
	"context"
	"errors"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Row states.
const (
	StateAdding  = "adding"
	StateReady   = "ready"
	StateMissing = "missing"
)

const (
	kindPath = "path"
	KindXFS  = "xfs"
)

// Host runs one disk command on a node.
type Host interface {
	Disk(ctx context.Context, nodeID string, r hostdisk.Request) (hostdisk.Result, error)
}

// MissingError: the node's disk is not ready, so nothing was done.
type MissingError struct{ Node string }

func (e *MissingError) Error() string { return "Its disk is missing." }
func (e *MissingError) Unwrap() error { return errs.ErrBusy }

type Leaf struct {
	disks   store.NodeDiskStore
	folders store.DiskFolderStore
	host    Host
	caps    func(ctx context.Context, name string) (int64, error)
	// capPending: Docker volume names whose cap the host may not have applied
	// (a timeout or a lost answer). In memory: the next folder run puts the
	// cap on again and clears it.
	capPending sync.Map
}

// WithCaps sets how the leaf learns a folder's size cap in bytes (0 = none),
// by the Docker volume name. Only an xfs disk enforces it.
func (l *Leaf) WithCaps(f func(ctx context.Context, name string) (int64, error)) *Leaf {
	l.caps = f
	return l
}

func New(disks store.NodeDiskStore, folders store.DiskFolderStore, h Host) *Leaf {
	return &Leaf{disks: disks, folders: folders, host: h}
}

// BindOpts are the Docker volume options that bind a volume to its folder.
func BindOpts(mount, diskID, name string) map[string]string {
	return docker.BindOpts(path.Join(mount, diskID, name))
}

// OfNode is the node's disk row; ok is false when it has none. An adopting
// row is also the disk of its target node (op_node_id), so that node already
// sees its disk as not ready.
// ponytail: the adopting lookup lists every disk row (a handful); add a
// lookup by op_node_id if a panel ever has thousands.
func (l *Leaf) OfNode(ctx context.Context, nodeID string) (store.NodeDisk, bool, error) {
	d, err := l.disks.GetByNode(ctx, nodeID)
	if err == nil {
		return d, true, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return d, false, err
	}
	rows, err := l.disks.List(ctx)
	if err != nil {
		return store.NodeDisk{}, false, err
	}
	for _, r := range rows {
		if r.State == StateAdopting && r.OpNodeID == nodeID {
			return r, true, nil
		}
	}
	return store.NodeDisk{}, false, nil
}

func (l *Leaf) req(d store.NodeDisk, op string, names ...string) hostdisk.Request {
	return hostdisk.Request{Op: op, Kind: d.Kind, DiskID: d.ID, Owner: d.NodeID, Mount: d.MountPath, Names: names}
}

// run sends r and turns a refusal into an error. disk_missing on a ready row
// latches it missing and returns *MissingError.
func (l *Leaf) run(ctx context.Context, d store.NodeDisk, r hostdisk.Request) (hostdisk.Result, error) {
	res, err := l.host.Disk(ctx, d.NodeID, r)
	if err != nil {
		return res, err
	}
	if res.Error == nil {
		return res, nil
	}
	if res.Error.Code == hostdisk.CodeDiskMissing {
		if _, err := l.MarkMissing(ctx, d); err != nil {
			return res, err
		}
		return res, &MissingError{Node: d.NodeID}
	}
	return res, refusal(res)
}

// live re-reads the row: a latch set by another call must stop this one.
func (l *Leaf) live(ctx context.Context, d store.NodeDisk) (store.NodeDisk, error) {
	cur, err := l.disks.Get(ctx, d.ID)
	if errors.Is(err, errs.ErrNotFound) {
		return d, &MissingError{Node: d.NodeID}
	}
	if err != nil {
		return d, err
	}
	if cur.State != StateReady {
		// the owner of an adopt (WithOp) works on the adopting row
		if m, ok := OpOf(ctx, cur); !ok || m != StateAdopting {
			return cur, &MissingError{Node: cur.NodeID}
		}
	}
	return cur, nil
}

// Probe reads a path on the node and writes nothing.
func (l *Leaf) Probe(ctx context.Context, nodeID, mount string) (hostdisk.Fact, error) {
	res, err := l.host.Disk(ctx, nodeID, hostdisk.Request{Op: "probe", Kind: kindPath, Mount: mount})
	if err != nil {
		return hostdisk.Fact{}, err
	}
	if res.Error != nil {
		return hostdisk.Fact{}, res.Error
	}
	if res.Disk == nil {
		return hostdisk.Fact{}, errors.New("disk: probe returned no fact")
	}
	return *res.Disk, nil
}

// AddPath writes the row in adding, has the host stamp the disk, then sets
// ready. A retry resumes an adding row of this node at the same path; an
// adding row at another path is a half-made add and is replaced. A refusal
// removes the adding row, a crash or an I/O error keeps it. A ready or missing row, or one
// with folders, is never touched: "This node already has a disk."
func (l *Leaf) AddPath(ctx context.Context, nodeID, mount, installID string) (store.NodeDisk, error) {
	d, ok, err := l.OfNode(ctx, nodeID)
	if err != nil {
		return d, err
	}
	switch {
	case ok && d.State != StateAdding:
		return d, errs.Conflictf("This node already has a disk.")
	case ok && d.MountPath != mount:
		if err := l.dropAdding(ctx, d); err != nil {
			return d, err
		}
		ok = false
	}
	if !ok {
		d = store.NodeDisk{ID: uuid.NewString(), NodeID: nodeID, Kind: kindPath, MountPath: mount, State: StateAdding, CreatedAt: time.Now().UTC()}
		if err := l.disks.Create(ctx, d); err != nil {
			return d, err
		}
	}
	r := l.req(d, "add")
	r.Install = installID
	res, err := l.host.Disk(ctx, nodeID, r)
	if err != nil {
		return d, err // a crash leaves the row for a retry
	}
	if res.Error != nil {
		if res.Error.Code != hostdisk.CodeIO { // an I/O error is not a refusal: the row stays for a retry
			_ = l.dropAdding(context.WithoutCancel(ctx), d)
		}
		return d, res.Error
	}
	d.State = StateReady
	return d, l.disks.Update(ctx, d)
}

// dropAdding deletes an adding row that has no folder rows. Anything else is
// left alone and refused.
func (l *Leaf) dropAdding(ctx context.Context, d store.NodeDisk) error {
	fs, err := l.folders.ListByDisk(ctx, d.ID)
	if err != nil {
		return err
	}
	if d.State != StateAdding || len(fs) > 0 {
		return errs.Conflictf("This node already has a disk.")
	}
	return l.disks.Delete(ctx, d.ID)
}

// Verify runs the host verify and sets ready, or missing on disk_missing.
// Other refusals change nothing. Only ready, adding and missing rows move.
func (l *Leaf) Verify(ctx context.Context, d store.NodeDisk) (store.NodeDisk, error) {
	cur, err := l.disks.Get(ctx, d.ID)
	if err != nil {
		return d, err
	}
	switch cur.State {
	case StateReady, StateAdding, StateMissing:
	default:
		return cur, nil
	}
	res, err := l.host.Disk(ctx, cur.NodeID, l.req(cur, "verify"))
	if err != nil {
		return cur, err
	}
	if res.Error != nil {
		if res.Error.Code == hostdisk.CodeDiskMissing && cur.State != StateAdding {
			return l.MarkMissing(ctx, cur)
		}
		return cur, res.Error
	}
	cur.State = StateReady
	return cur, l.disks.Update(ctx, cur)
}

// MarkMissing latches a ready row missing. Any other state is left alone, so
// it can never overwrite the state of an unfinished operation.
func (l *Leaf) MarkMissing(ctx context.Context, d store.NodeDisk) (store.NodeDisk, error) {
	cur, err := l.disks.Get(ctx, d.ID)
	if err != nil || cur.State != StateReady {
		return cur, err
	}
	cur.State = StateMissing
	return cur, l.disks.Update(ctx, cur)
}

// Folders are the disk's folder rows.
func (l *Leaf) Folders(ctx context.Context, d store.NodeDisk) ([]store.DiskFolder, error) {
	return l.folders.ListByDisk(ctx, d.ID)
}

// HasFolders reports which names have a folder row. A DB read.
func (l *Leaf) HasFolders(ctx context.Context, d store.NodeDisk, names ...string) (map[string]bool, error) {
	rows, err := l.folders.ListByDisk(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, f := range rows {
		have[f.Name] = true
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = have[n]
	}
	return out, nil
}

// Place is get-or-create: the folder row, then the folder. A lost race on the
// row is not an error. A refused folder removes a row this call made. On an
// xfs disk a new row takes the next project number of the disk and the folder
// is put into it with the volume's cap.
func (l *Leaf) Place(ctx context.Context, d store.NodeDisk, name string) (map[string]string, error) {
	d, err := l.live(ctx, d)
	if err != nil {
		return nil, err
	}
	made := true
	row := store.DiskFolder{ID: uuid.NewString(), DiskID: d.ID, Name: name, CreatedAt: time.Now().UTC()}
	for try := 0; ; try++ {
		if d.Kind == KindXFS {
			rows, err := l.folders.ListByDisk(ctx, d.ID)
			if err != nil {
				return nil, err
			}
			if i := slices.IndexFunc(rows, func(f store.DiskFolder) bool { return f.Name == name }); i >= 0 {
				made = false
				break
			}
			row.Project = 1
			for _, f := range rows {
				row.Project = max(row.Project, f.Project+1)
			}
		}
		err := l.folders.Create(ctx, row)
		if err == nil {
			break
		}
		if _, ok := errs.IsConflict(err); !ok {
			return nil, err
		}
		if d.Kind != KindXFS {
			made = false
			break
		}
		// the name or the project number was taken by a concurrent place: read again
		if try >= 5 {
			return nil, err
		}
	}
	r, err := l.reqQ(ctx, d, "folder", name)
	if err == nil {
		_, err = l.run(ctx, d, r)
	}
	if err == nil && d.Kind == KindXFS {
		l.capPending.Delete(name)
	}
	if err != nil {
		if made {
			_ = l.folders.Delete(context.WithoutCancel(ctx), row.ID)
		}
		return nil, err
	}
	return BindOpts(d.MountPath, d.ID, name), nil
}

// Usage asks the host; a failure is an error, never a zero.
func (l *Leaf) Usage(ctx context.Context, d store.NodeDisk, sizes bool, names ...string) (map[string]hostdisk.Use, error) {
	d, err := l.live(ctx, d)
	if err != nil {
		return nil, err
	}
	if d.Kind == KindXFS && sizes {
		// the sizes come from the projects: a name with no folder row has none
		rows, err := l.folders.ListByDisk(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		names = slices.DeleteFunc(slices.Clone(names), func(n string) bool {
			return !slices.ContainsFunc(rows, func(f store.DiskFolder) bool { return f.Name == n })
		})
		if len(names) == 0 {
			return map[string]hostdisk.Use{}, nil
		}
	}
	r, err := l.reqQ(ctx, d, "usage", names...)
	if err != nil {
		return nil, err
	}
	r.Sizes = sizes
	res, err := l.run(ctx, d, r)
	if err != nil {
		return nil, err
	}
	if res.Usage == nil {
		return map[string]hostdisk.Use{}, nil
	}
	return res.Usage, nil
}

// Drop removes the folder on the host, then its row. A failure keeps the row.
func (l *Leaf) Drop(ctx context.Context, d store.NodeDisk, name string) error {
	d, err := l.live(ctx, d)
	if err != nil {
		return err
	}
	if _, err := l.run(ctx, d, l.req(d, "drop", name)); err != nil {
		return err
	}
	rows, err := l.folders.ListByDisk(ctx, d.ID)
	if err != nil {
		return err
	}
	for _, f := range rows {
		if f.Name == name {
			return l.folders.Delete(ctx, f.ID)
		}
	}
	return nil
}

// Delete removes the disk row and, by cascade, its folder rows. Rows only.
func (l *Leaf) Delete(ctx context.Context, d store.NodeDisk) error {
	return l.disks.Delete(ctx, d.ID)
}
