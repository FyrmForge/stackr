package disk

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The xfs kind: a block device stackr formats, mounts and hands out in
// folders, each folder an xfs project with a hard cap.

func kindOf(d store.NodeDisk) string {
	if d.Kind == "" {
		return kindPath
	}
	return d.Kind
}

// MsgNeedsForce refuses a device that holds a filesystem, a partition table or
// an earlier stackr disk, until the person chooses Format anyway.
const MsgNeedsForce = "That disk already holds data. Use Format anyway to erase it."

// Refusal is a host refusal in words for the person; the hostdisk error stays
// under it for callers that read the code.
type Refusal struct {
	Msg string
	Err *hostdisk.Error
}

func (e *Refusal) Error() string   { return e.Msg }
func (e *Refusal) Unwrap() []error { return []error{errs.Refused{Msg: e.Msg}, e.Err} }

// BlockedWhy says why a device cannot be formatted (hostdisk.Device.Blocked).
func BlockedWhy(blocked string) string {
	switch blocked {
	case "system":
		return "it holds the system or Docker"
	case "mounted":
		return "it is in use (mounted)"
	case "partitions":
		return "it has partitions"
	case "swap":
		return "it is used as swap"
	case "lvm":
		return "it is part of an LVM volume group"
	case "md":
		return "it is part of a RAID array"
	case "luks":
		return "it is an encrypted container"
	case "zfs":
		return "it is part of a ZFS pool"
	case "holders":
		return "another device is built on it"
	case "readonly":
		return "it is read-only"
	}
	return "it is not available"
}

// refusal turns a host result's error into the words the panel shows. Codes of
// the path kind and I/O errors stay as they are.
func refusal(res hostdisk.Result) error {
	e := res.Error
	msg := ""
	switch e.Code {
	case hostdisk.CodeNoDevice:
		msg = "That disk is not on this node any more. Refresh the list."
	case hostdisk.CodeDeviceChanged:
		msg = "That disk changed since it was listed. Refresh the list and pick it again."
	case hostdisk.CodeDeviceBlocked:
		why := "it is not available"
		if len(res.Devices) > 0 {
			why = BlockedWhy(res.Devices[0].Blocked)
		}
		msg = "That disk cannot be used: " + why + "."
	case hostdisk.CodeNeedsForce:
		msg = MsgNeedsForce
	case hostdisk.CodeDeviceSmaller:
		msg = "The device is smaller than when the disk was added. Nothing was grown."
	case hostdisk.CodeCapBelowUse:
		msg = "The volume already holds more than that cap. Set a larger cap."
	case hostdisk.CodeBusy:
		msg = "The mount point or the disk is in use by something else."
	default:
		return e
	}
	return &Refusal{Msg: msg, Err: e}
}

// reqQ is req plus, for an xfs disk, each named folder's project and cap.
// A name with no folder row is left out.
func (l *Leaf) reqQ(ctx context.Context, d store.NodeDisk, op string, names ...string) (hostdisk.Request, error) {
	r := l.req(d, op, names...)
	if d.Kind != KindXFS {
		return r, nil
	}
	rows, err := l.folders.ListByDisk(ctx, d.ID)
	if err != nil {
		return r, err
	}
	r.Quotas = map[string]hostdisk.Quota{}
	for _, n := range names {
		for _, f := range rows {
			if f.Name != n {
				continue
			}
			q := hostdisk.Quota{Project: uint32(f.Project)}
			if l.caps != nil {
				if q.Cap, err = l.caps(ctx, n); err != nil {
					return r, err
				}
			}
			r.Quotas[n] = q
		}
	}
	return r, nil
}

// Devices lists the node's whole disks with why each can or cannot be used.
func (l *Leaf) Devices(ctx context.Context, nodeID string) ([]hostdisk.Device, error) {
	res, err := l.host.Disk(ctx, nodeID, hostdisk.Request{Op: "list", Kind: KindXFS})
	if err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, refusal(res)
	}
	return res.Devices, nil
}

// device is the listed device with this id or path.
func device(devs []hostdisk.Device, id string) (hostdisk.Device, bool) {
	for _, x := range devs {
		if x.ID == id || x.Path == id {
			return x, true
		}
	}
	return hostdisk.Device{}, false
}

// FindDevice is the device the node lists under id.
func (l *Leaf) FindDevice(ctx context.Context, nodeID, id string) (hostdisk.Device, error) {
	devs, err := l.Devices(ctx, nodeID)
	if err != nil {
		return hostdisk.Device{}, err
	}
	x, ok := device(devs, id)
	if !ok {
		return x, refusal(hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeNoDevice, Message: "no device " + id}})
	}
	return x, nil
}

// AddXFS makes the device the node's disk in three host runs, because a
// mount made in the host's namespace is not seen by the run that made it:
// list and format (the device must still be the one pick shows), list again
// and mount, then the stamp. The adding row carries the device, and the
// filesystem uuid once mkfs has run; a retry of the same device with a
// filesystem uuid goes on from the mount and never formats again. A refusal
// before the format removes the row; a crash or an I/O error keeps it.
func (l *Leaf) AddXFS(ctx context.Context, nodeID string, pick hostdisk.Device, force bool, installID string) (store.NodeDisk, error) {
	d, ok, err := l.OfNode(ctx, nodeID)
	if err != nil {
		return d, err
	}
	switch {
	case ok && d.State != StateAdding:
		return d, errs.Conflictf("This node already has a disk.")
	case ok && (d.Kind != KindXFS || d.DeviceID != pick.ID),
		ok && d.FSUUID != "" && d.FSUUID != pick.FSUUID:
		// another device, or the same one with another filesystem now (the row's
		// uuid is stuck: nothing can mount it): start over, the typed confirm is fresh
		if err := l.dropAdding(ctx, d); err != nil {
			return d, err
		}
		ok = false
	}
	if !ok {
		id := uuid.NewString()
		d = store.NodeDisk{ID: id, NodeID: nodeID, Kind: KindXFS, MountPath: hostdisk.MountPoint(id), State: StateAdding,
			DeviceID: pick.ID, DeviceBytes: pick.Bytes, CreatedAt: time.Now().UTC()}
		if err := l.disks.Create(ctx, d); err != nil {
			return d, err
		}
	}
	// a refusal before the filesystem exists leaves nothing: the row goes
	fail := func(err error) (store.NodeDisk, error) {
		var he *hostdisk.Error
		if d.FSUUID == "" && errors.As(err, &he) && he.Code != hostdisk.CodeIO {
			_ = l.dropAdding(context.WithoutCancel(ctx), d)
		}
		return d, err
	}
	if d.FSUUID == "" {
		cur, err := l.FindDevice(ctx, nodeID, d.DeviceID)
		if err != nil {
			return fail(err)
		}
		if !hostdisk.SameDevice(cur, pick) {
			return fail(refusal(hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeDeviceChanged, Message: d.DeviceID + " is not the device that was picked"}}))
		}
		res, err := l.host.Disk(ctx, nodeID, hostdisk.Request{Op: "format", Kind: KindXFS, Device: pick.ID, Expect: &pick, Force: force})
		if err != nil {
			return d, err
		}
		if res.Error != nil {
			return fail(refusal(res))
		}
		if len(res.Devices) == 0 || res.Devices[0].FSUUID == "" {
			return d, errors.New("disk: format returned no filesystem")
		}
		d.FSUUID, d.DeviceBytes = res.Devices[0].FSUUID, res.Devices[0].Bytes
		if err := l.disks.Update(ctx, d); err != nil {
			return d, err
		}
	}
	fresh, err := l.FindDevice(ctx, nodeID, d.DeviceID)
	if err != nil {
		return fail(err)
	}
	if fresh.FSUUID != d.FSUUID {
		return fail(refusal(hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeDeviceChanged, Message: d.DeviceID + " is not the formatted device"}}))
	}
	res, err := l.host.Disk(ctx, nodeID, hostdisk.Request{Op: "mount", Kind: KindXFS, DiskID: d.ID, Mount: d.MountPath, Device: fresh.ID, Expect: &fresh})
	if err != nil {
		return d, err
	}
	if res.Error != nil {
		return fail(refusal(res))
	}
	r := l.req(d, "add")
	r.Install = installID
	if res, err = l.host.Disk(ctx, nodeID, r); err != nil {
		return d, err
	}
	if res.Error != nil {
		return fail(refusal(res))
	}
	if f := res.Disk; f != nil {
		if f.Device != "" {
			d.DeviceID = f.Device
		}
		if f.DeviceBytes > 0 {
			d.DeviceBytes = f.DeviceBytes
		}
		if f.FSUUID != "" {
			d.FSUUID = f.FSUUID
		}
	}
	d.State = StateReady
	return d, l.disks.Update(ctx, d)
}

// MountXFS mounts the device of an xfs disk on the node it is moving to (an
// adopt): the device whose filesystem uuid is the disk's. Idempotent.
func (l *Leaf) MountXFS(ctx context.Context, nodeID string, d store.NodeDisk, mount string) error {
	if d.FSUUID == "" {
		return errs.Refusedf("This disk has no recorded filesystem.")
	}
	devs, err := l.Devices(ctx, nodeID)
	if err != nil {
		return err
	}
	for _, x := range devs {
		if x.FSUUID == d.FSUUID {
			res, err := l.host.Disk(ctx, nodeID, hostdisk.Request{Op: "mount", Kind: KindXFS, DiskID: d.ID, Mount: mount, Device: x.ID, Expect: &x})
			if err != nil {
				return err
			}
			if res.Error != nil {
				return refusal(res)
			}
			return nil
		}
	}
	return refusal(hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeNoDevice, Message: "no device with filesystem " + d.FSUUID}})
}

// UnmountXFS takes an xfs disk off its node: unmounted, its mount unit
// removed, the mount point removed. The device and the folders on it are not
// touched. Idempotent; refused while something else is mounted below it.
func (l *Leaf) UnmountXFS(ctx context.Context, d store.NodeDisk) error {
	if d.Kind != KindXFS {
		return nil
	}
	res, err := l.host.Disk(ctx, d.NodeID, hostdisk.Request{Op: "unmount", Kind: KindXFS, DiskID: d.ID, Mount: d.MountPath})
	if err != nil {
		return err
	}
	if res.Error != nil {
		return refusal(res)
	}
	return nil
}

// HasDevice says whether the node lists a device carrying the disk's filesystem.
func (l *Leaf) HasDevice(ctx context.Context, nodeID string, d store.NodeDisk) (bool, error) {
	devs, err := l.Devices(ctx, nodeID)
	if err != nil {
		return false, err
	}
	for _, x := range devs {
		if x.FSUUID != "" && x.FSUUID == d.FSUUID {
			return true, nil
		}
	}
	return false, nil
}

// Grow has the host grow the filesystem to its device and records the
// device's new size. A device smaller than recorded is refused.
func (l *Leaf) Grow(ctx context.Context, d store.NodeDisk) (store.NodeDisk, error) {
	d, err := l.live(ctx, d)
	if err != nil {
		return d, err
	}
	if d.Kind != KindXFS {
		return d, errs.Refusedf("Only a disk made from a whole device can grow.")
	}
	r := l.req(d, "grow")
	r.DeviceBytes, r.FSUUID = d.DeviceBytes, d.FSUUID
	res, err := l.run(ctx, d, r)
	if err != nil {
		return d, err
	}
	if f := res.Disk; f != nil && f.DeviceBytes > 0 {
		d.DeviceBytes = f.DeviceBytes
		if err := l.disks.Update(ctx, d); err != nil {
			return d, err
		}
		l.grown.Store(d.ID, time.Now())
	}
	return d, nil
}

// SetCap puts a folder's cap, read through the caps seam, on the host: the
// caller saves the new value first. A cap under what the folder holds is
// refused by the host before anything changes.
func (l *Leaf) SetCap(ctx context.Context, d store.NodeDisk, name string) error {
	d, err := l.live(ctx, d)
	if err != nil {
		return err
	}
	if d.Kind != KindXFS {
		return nil
	}
	r, err := l.reqQ(ctx, d, "folder", name)
	if err != nil {
		return err
	}
	if _, ok := r.Quotas[name]; !ok {
		return nil // no folder yet: Place applies the cap
	}
	if _, err = l.run(ctx, d, r); err != nil {
		if Ambiguous(err) {
			l.capPending.Store(name, struct{}{})
		}
		return err
	}
	l.capPending.Delete(name)
	return nil
}

// Ambiguous says whether a failed host call may still have changed something:
// only a refusal the helper returns before it changes anything (and a disk
// that is missing) is definitive. A timeout, a lost answer or an I/O error
// halfway is not.
func Ambiguous(err error) bool {
	var he *hostdisk.Error
	var miss *MissingError
	switch {
	case err == nil:
		return false
	case errors.As(err, &miss), errors.Is(err, errs.ErrRefused):
		return false
	case errors.As(err, &he):
		return he.Code == hostdisk.CodeIO
	}
	return true
}

// CapPending says whether the cap of the volume with this Docker name may not
// be on the host yet. A memory read, never a host call.
func (l *Leaf) CapPending(name string) bool {
	_, ok := l.capPending.Load(name)
	return ok
}
