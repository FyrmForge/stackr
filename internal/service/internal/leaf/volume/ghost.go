package volume

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The object-only remove. It takes a Docker volume object away without
// touching a folder or a folder row: forget disk (step 3) and the two Discard
// verbs of a ghost go through here, and each guards the call by what the
// object is.

// Ghost is a Docker volume that shadows the folder row of a disk volume.
// Device is where the object points, "" for a plain copy; Want is the folder
// the volume belongs on.
type Ghost struct{ Node, Name, Device, Want string }

// DiscardGhost removes the Docker object of v when it is a ghost: a plain
// copy when bound is false, an object bound elsewhere when it is true. check
// sees the ghost first and may refuse (the typed confirm). The folder and its
// row stay.
func (l *Leaf) DiscardGhost(ctx context.Context, v store.Volume, bound bool, check func(Ghost) error) (Ghost, error) {
	cctx, node, err := l.at(ctx, v)
	if err != nil {
		return Ghost{}, err
	}
	st, err := l.state(cctx, node)
	if err != nil {
		return Ghost{}, err
	}
	switch st {
	case DiskNone:
		return Ghost{}, errs.Refusedf("This volume has no second copy.")
	case DiskNotReady:
		return Ghost{}, &MissingError{Node: node}
	}
	p, err := l.resolve(cctx, node, v.Name, st)
	if err != nil {
		return Ghost{}, err
	}
	if p.rt != RouteGhost {
		return Ghost{}, errs.Refusedf("This volume has no second copy.")
	}
	want, err := l.disk.Path(cctx, node, v.Name)
	if err != nil {
		return Ghost{}, err
	}
	g := Ghost{Node: node, Name: v.Name, Device: p.obj.Device, Want: want}
	switch {
	case bound && g.Device == "":
		return g, errs.Refusedf("This is a plain copy. Keep the disk copy instead.")
	case !bound && g.Device != "":
		return g, errs.Refusedf("This object is bound elsewhere. Remove the stray object instead.")
	}
	if check != nil {
		if err := check(g); err != nil {
			return g, err
		}
	}
	return g, l.docker.RemoveVolume(cctx, g.Name)
}

// RemoveBoundTo removes the Docker object name on nodeID when it is bound to
// device, and nothing else: an absent object, a plain one or one bound
// elsewhere is left alone (it may hold data restored after the disk left).
func (l *Leaf) RemoveBoundTo(ctx context.Context, nodeID, name, device string) (removed bool, err error) {
	ctx = docker.WithNode(ctx, nodeID)
	found, dev, err := l.docker.VolumeDevice(ctx, name)
	if err != nil || !found || dev != device {
		return false, err
	}
	return true, l.docker.RemoveVolume(ctx, name)
}
