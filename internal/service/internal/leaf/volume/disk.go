package volume

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DiskState is the node's disk as the volume leaf sees it. DiskNone is no
// node_disks row, and nothing else: any other stored state (adding, missing,
// later adopting and forgetting) is DiskNotReady, a disk that exists and
// cannot be trusted right now.
type DiskState int

const (
	DiskNone DiskState = iota
	DiskReady
	DiskNotReady
)

// UseMode is how fresh a Usage answer must be.
type UseMode int

const (
	UseEmpty  UseMode = iota // readdir only
	UseCached                // a short TTL cache
	UseFresh                 // a helper call
)

// Use is one folder's usage. A name with no folder is Missing.
type Use struct {
	Bytes          int64
	Empty, Missing bool
}

// Disk is the node disk as volumes need it; the service implements it over
// leaf/disk (a leaf cannot import a leaf).
type Disk interface {
	// State is a DB read: no Docker call, no helper run. It is asked first,
	// always. An error is returned as an error and is never DiskNone.
	State(ctx context.Context, nodeID string) (DiskState, error)
	// Folders reports which names have a folder row on the node's disk. A DB read.
	Folders(ctx context.Context, nodeID string, names ...string) (map[string]bool, error)
	// Path is the folder a volume of this name is bound to on the node's ready
	// disk. Pure: no host call, no row written. *MissingError unless the disk is ready.
	Path(ctx context.Context, nodeID, name string) (string, error)
	// Place is get-or-create: the folder row, then the folder. *MissingError unless the disk is ready.
	Place(ctx context.Context, nodeID, name string) (opts map[string]string, err error)
	// Usage: *MissingError unless the disk is ready.
	Usage(ctx context.Context, nodeID string, mode UseMode, names ...string) (map[string]Use, error)
	// Drop: *MissingError unless the disk is ready.
	Drop(ctx context.Context, nodeID, name string) error
}

// MissingError is a volume action refused because the node's disk is not
// ready. Nothing was created, measured or removed.
type MissingError struct{ Node string }

func (e *MissingError) Error() string { return "Its disk is missing." }
func (e *MissingError) Unwrap() error { return errs.ErrBusy }

// GhostError is a Docker volume that shadows the folder it should be bound
// to: a plain copy, or one bound elsewhere (Device is then its folder).
type GhostError struct{ Name, Device string }

func (e *GhostError) Error() string { return "volume exists outside the disk" }
func (e *GhostError) Unwrap() error { return errs.ErrBusy }

// Object is a Docker volume as inspected; Found with Device "" is a plain one.
type Object struct {
	Found  bool
	Device string
}

type Route int

const (
	RouteLegacy Route = iota
	RouteDisk
	RouteMissing
	RouteGhost
)

// route is the decision table. want is the folder path for this name; it is
// only read for a bound object on a ready disk.
func route(st DiskState, hasRow bool, obj Object, want string) Route {
	switch st {
	case DiskNone:
		return RouteLegacy
	case DiskNotReady:
		if hasRow || !obj.Found || obj.Device != "" {
			return RouteMissing
		}
		return RouteLegacy // a plain volume with no folder row stays legacy
	}
	switch {
	case !obj.Found:
		return RouteDisk
	case obj.Device == "":
		if hasRow {
			return RouteGhost
		}
		return RouteLegacy
	case obj.Device == want:
		return RouteDisk
	}
	return RouteGhost
}

// state is the first question every verb asks. No disk seam, or no node to
// ask about, is DiskNone; a lookup error is an error, never DiskNone.
func (l *Leaf) state(ctx context.Context, nodeID string) (DiskState, error) {
	if l.disk == nil || nodeID == "" {
		return DiskNone, nil
	}
	return l.disk.State(ctx, nodeID)
}

// WithDisk sets the node disk seam.
func (l *Leaf) WithDisk(d Disk) *Leaf {
	l.disk = d
	return l
}

type placement struct {
	rt     Route
	hasRow bool
	obj    Object
	opts   map[string]string // from Place, when it ran
}

// resolve routes one name on a node whose state is not DiskNone.
func (l *Leaf) resolve(ctx context.Context, nodeID, name string, st DiskState) (p placement, err error) {
	rows, err := l.disk.Folders(ctx, nodeID, name)
	if err != nil {
		return p, err
	}
	p.hasRow = rows[name]
	found, dev, err := l.docker.VolumeDevice(ctx, name)
	if err != nil {
		return p, err
	}
	p.obj = Object{Found: found, Device: dev}
	want := ""
	if st == DiskReady && dev != "" { // a read never places: only EnsureNamed does
		if want, err = l.disk.Path(ctx, nodeID, name); err != nil {
			return p, err
		}
	}
	p.rt = route(st, p.hasRow, p.obj, want)
	return p, nil
}

// refuse is the error a Missing or Ghost route answers every verb with.
func (p placement) refuse(nodeID, name string) error {
	switch p.rt {
	case RouteMissing:
		return &MissingError{Node: nodeID}
	case RouteGhost:
		return &GhostError{Name: name, Device: p.obj.Device}
	}
	return nil
}

// EnsureNamed makes the Docker volume name on nodeID; an existing one is fine.
func (l *Leaf) EnsureNamed(ctx context.Context, nodeID, name string, labels map[string]string) error {
	if nodeID != "" {
		ctx = docker.WithNode(ctx, nodeID)
	}
	st, err := l.state(ctx, nodeID)
	if err != nil {
		return err
	}
	if st == DiskNone {
		return l.docker.CreateVolume(ctx, name, "local", nil, labels)
	}
	p, err := l.resolve(ctx, nodeID, name, st)
	if err != nil {
		return err
	}
	switch p.rt {
	case RouteLegacy:
		return l.docker.CreateVolume(ctx, name, "local", nil, labels)
	case RouteGhost:
		// An empty plain ghost goes (Docker's size is reliable for a plain
		// volume and the folder is the truth); anything else is the admin's call.
		if p.obj.Device != "" {
			return p.refuse(nodeID, name)
		}
		info, err := l.docker.InspectVolume(ctx, name)
		if err != nil || info.SizeBytes != 0 {
			return p.refuse(nodeID, name)
		}
		if err := l.docker.RemoveVolume(ctx, name); err != nil {
			return err
		}
	case RouteMissing:
		return p.refuse(nodeID, name)
	}
	if p.opts == nil {
		if p.opts, err = l.disk.Place(ctx, nodeID, name); err != nil {
			return err
		}
	}
	return l.docker.CreateVolume(ctx, name, "local", p.opts, labels)
}

// RemoveNamed removes the Docker volume name on nodeID and, for a disk
// volume, its folder and folder row. An absent volume is fine.
func (l *Leaf) RemoveNamed(ctx context.Context, nodeID, name string) error {
	if nodeID != "" {
		ctx = docker.WithNode(ctx, nodeID)
	}
	st, err := l.state(ctx, nodeID)
	if err != nil {
		return err
	}
	if st == DiskNone {
		return l.docker.RemoveVolume(ctx, name)
	}
	p, err := l.resolve(ctx, nodeID, name, st)
	if err != nil {
		return err
	}
	if !p.obj.Found && !p.hasRow {
		return nil // nothing to remove, whatever the disk state
	}
	switch p.rt {
	case RouteLegacy:
		return l.docker.RemoveVolume(ctx, name)
	case RouteDisk:
		if err := l.docker.RemoveVolume(ctx, name); err != nil {
			return err
		}
		return l.disk.Drop(ctx, nodeID, name)
	}
	return p.refuse(nodeID, name)
}

// OnDisk is whether the volume has a folder on its node's disk. A read: it
// asks the rows only, never Docker or the host.
func (l *Leaf) OnDisk(ctx context.Context, v store.Volume) bool {
	_, node, err := l.at(ctx, v)
	if err != nil {
		return false
	}
	if st, err := l.state(ctx, node); err != nil || st == DiskNone {
		return false
	}
	rows, err := l.disk.Folders(ctx, node, v.Name)
	return err == nil && rows[v.Name]
}

// Where is where a volume lives, for display: RouteDisk when its node's disk
// has a folder row for it, else RouteLegacy, and the node. Rows only: no Docker
// call, so it cannot tell a ghost (two copies); deploy and backup gates do. A
// failed row read is an error, never a guess.
func (l *Leaf) Where(ctx context.Context, v store.Volume) (Route, string, error) {
	_, node, err := l.at(ctx, v)
	if err != nil {
		return RouteLegacy, "", err
	}
	st, err := l.state(ctx, node)
	if err != nil {
		return RouteLegacy, node, err
	}
	if st == DiskNone {
		return RouteLegacy, node, nil
	}
	rows, err := l.disk.Folders(ctx, node, v.Name)
	if err != nil {
		return RouteLegacy, node, err
	}
	if !rows[v.Name] {
		return RouteLegacy, node, nil
	}
	return RouteDisk, node, nil
}
