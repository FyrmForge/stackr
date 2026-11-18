package hostdisk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// XFSRoot holds the mount points of xfs disks, one per disk id. The panel
// sees it read-only (rslave bind), the mount itself is made in the host's
// mount namespace.
const XFSRoot = "/var/lib/stackr-disks"

// MountPoint is where the xfs disk with this id is mounted.
func MountPoint(diskID string) string { return XFSRoot + "/" + diskID }

// xfsKind is a raw block device that stackr formats (mkfs.xfs, never -f),
// mounts and hands out in folders, each folder an xfs project with a hard
// cap. A disk is added in three helper runs, because a mount made in the
// host namespace is not visible to the run that made it: format, mount, add
// (the stamp). Every later op runs against the mount like path does.
type xfsKind struct{}

func (xfsKind) run(ctx context.Context, e Env, r Request) Result {
	switch r.Op {
	case "list":
		return list(ctx, e)
	case "format":
		return xfsFormat(ctx, e, r)
	case "mount":
		return xfsMountOp(ctx, e, r)
	case "unmount":
		return xfsUnmount(ctx, e, r)
	case "add":
		return e.guarded(r, func() Result {
			res := addStamp(e, r, "xfs", func(t *target) *Error {
				if t.mount.FSType != "xfs" {
					return &Error{CodeDiskMissing, r.Mount + " is not an xfs filesystem"}
				}
				return nil
			})
			return e.decorate(ctx, res, r.Mount)
		})
	case "verify":
		return e.guarded(r, func() Result { return e.decorate(ctx, verify(e, r), r.Mount) })
	case "claim":
		return e.guarded(r, func() Result { return claim(e, r) })
	case "drop":
		return e.guarded(r, func() Result { return drop(e, r) })
	case "folder":
		return e.guarded(r, func() Result { return xfsFolder(ctx, e, r) })
	case "usage":
		return e.guarded(r, func() Result { return xfsUsage(ctx, e, r) })
	case "grow":
		return e.guarded(r, func() Result { return xfsGrow(ctx, e, r) })
	}
	return fail(CodeUnsupported, "op "+r.Op)
}

// guarded runs an op on a mounted xfs disk: the mount point is the one for
// this disk id and an xfs filesystem is mounted there.
func (e Env) guarded(r Request, op func() Result) Result {
	if herr := badName(r.DiskID); herr != nil {
		return Result{Error: herr}
	}
	if r.Mount != MountPoint(r.DiskID) {
		return fail(CodeBadName, "an xfs disk is mounted at "+MountPoint(r.DiskID))
	}
	table, err := os.ReadFile(e.mountinfo())
	if err != nil {
		return fail(CodeIO, "read the mount table: "+err.Error())
	}
	m, ok := lastCovering(ParseMounts(table), r.Mount)
	if !ok || m.Point != r.Mount {
		return fail(CodeDiskMissing, r.Mount+" is not mounted")
	}
	if m.FSType != "xfs" {
		return fail(CodeDiskMissing, r.Mount+" is not an xfs filesystem")
	}
	return op()
}

// pick finds the device a request names and requires it to be the one the
// list showed. The list is read again here, right before anything is done.
func (e Env) pick(ctx context.Context, r Request) (Device, *Error) {
	if r.Expect == nil {
		return Device{}, &Error{CodeDeviceChanged, "no device identity to check"}
	}
	devs, _, herr := e.devices(ctx)
	if herr != nil {
		return Device{}, herr
	}
	d, ok := find(devs, r.Device)
	if !ok {
		return Device{}, &Error{CodeNoDevice, "no device " + r.Device}
	}
	if !sameDevice(d, *r.Expect) {
		return d, &Error{CodeDeviceChanged, r.Device + " is not the device that was listed"}
	}
	return d, nil
}

// xfsFormat makes an xfs filesystem on a whole device. Refused with no
// override: anything in Device.Blocked. Refused without Force: a stackr
// stamp, a foreign filesystem, a partition table. Force wipes the signatures
// (wipefs) so mkfs, which never gets -f, can go on; mkfs refusing to
// overwrite is a second guard behind ours.
func xfsFormat(ctx context.Context, e Env, r Request) Result {
	dev, herr := e.pick(ctx, r)
	if herr != nil {
		return Result{Error: herr}
	}
	if dev.Blocked != "" {
		return Result{Error: &Error{CodeDeviceBlocked, dev.Path + " cannot be formatted: " + dev.Blocked}, Devices: []Device{dev}}
	}
	if dev.Signature != "" && !r.Force {
		return Result{Error: &Error{CodeNeedsForce, dev.Path + " holds a " + dev.Signature + " signature"}, Devices: []Device{dev}}
	}
	// Every destructive command runs on the stable by-id path when there is
	// one, and only after the device node is stat'ed again: the kernel name
	// can move to another disk between processes. A device with no by-id link
	// (virtio) is guarded by the same rdev, size and identity check.
	target := e.target(dev)
	if herr := e.recheck(ctx, dev, target, true); herr != nil {
		return Result{Error: herr}
	}
	if dev.Signature != "" {
		if _, err := e.exec(ctx, "wipefs", "-a", target); err != nil {
			return fail(CodeIO, err.Error())
		}
	}
	if herr := e.recheck(ctx, dev, target, false); herr != nil {
		return Result{Error: herr}
	}
	// -K: no discard, so mkfs does not trim the whole device before it starts.
	if _, err := e.exec(ctx, "mkfs.xfs", "-K", "-m", "reflink=1", "-L", stackrLabel, target); err != nil {
		return fail(CodeIO, err.Error())
	}
	devs, _, herr := e.devices(ctx)
	if herr != nil {
		return Result{Error: herr}
	}
	after, ok := find(devs, dev.Path)
	if !ok || after.FSUUID == "" || after.FSType != "xfs" {
		return fail(CodeIO, "the filesystem is not there after mkfs")
	}
	return Result{Devices: []Device{after}}
}

// target is the path a destructive command runs on: the by-id link when the
// device has one (read under Root, where the helper found it), else the node.
func (e Env) target(d Device) string {
	if d.ID != d.Path {
		return joinRoot(e.Root, d.ID)
	}
	return d.Path
}

// rdevOf is the "major:minor" of the block device node at p.
func (e Env) rdevOf(p string) (string, error) {
	if e.StatRdev != nil {
		return e.StatRdev(p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fi.Mode()&os.ModeDevice == 0 {
		return "", errors.New(p + " is not a device node")
	}
	r := uint64(st.Rdev)
	return fmt.Sprintf("%d:%d", (r>>8)&0xfff|(r>>32)&^0xfff, r&0xff|(r>>12)&^0xff), nil
}

// recheck is the guard right before a destructive command: the node at target
// is still the listed device (same major:minor), the list still shows the same
// disk (identity and size) and it is still not blocked. With full the
// filesystem and table fields must match too (before wipefs); after wipefs
// they are gone by design.
func (e Env) recheck(ctx context.Context, listed Device, target string, full bool) *Error {
	rdev, err := e.rdevOf(target)
	if err != nil {
		return &Error{CodeDeviceChanged, target + ": " + err.Error()}
	}
	if rdev != listed.MajMin {
		return &Error{CodeDeviceChanged, target + " is not the device that was listed"}
	}
	devs, _, herr := e.devices(ctx)
	if herr != nil {
		return herr
	}
	now, ok := find(devs, listed.ID)
	if !ok {
		return &Error{CodeDeviceChanged, listed.ID + " is gone"}
	}
	if !full {
		now.FSType, now.FSUUID, now.Label, now.PTType, now.PTUUID = listed.FSType, listed.FSUUID, listed.Label, listed.PTType, listed.PTUUID
	}
	if !sameDevice(now, listed) || now.Blocked != "" {
		return &Error{CodeDeviceChanged, listed.ID + " is not the device that was listed"}
	}
	return nil
}

var uuidRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// xfsMountOp mounts a formatted device at its mount point in the host's
// mount namespace and writes the systemd unit that does it at boot. It is
// idempotent: a device already mounted there is only checked. Expect.FSUUID
// is the filesystem to mount; the device must still be the one listed.
func xfsMountOp(ctx context.Context, e Env, r Request) Result {
	if herr := badName(r.DiskID); herr != nil {
		return Result{Error: herr}
	}
	point := MountPoint(r.DiskID)
	if r.Mount != point {
		return fail(CodeBadName, "an xfs disk is mounted at "+point)
	}
	dev, herr := e.pick(ctx, r)
	if herr != nil {
		return Result{Error: herr}
	}
	if !uuidRe.MatchString(dev.FSUUID) || dev.FSType != "xfs" || dev.Label != stackrLabel {
		return fail(CodeForeignStamp, dev.Path+" is not a stackr disk")
	}
	table, err := os.ReadFile(e.mountinfo())
	if err != nil {
		return fail(CodeIO, "read the mount table: "+err.Error())
	}
	// "Mounted here" is decided from the host's mount table, not from lsblk:
	// the helper's lsblk reports its mount points under /host.
	onlyHere := true
	for _, m := range ParseMounts(table) {
		if m.Source == dev.Path && m.Point != point {
			onlyHere = false
		}
	}
	for _, m := range dev.Mounts { // lsblk in the helper shows the host's mounts under /host
		if strings.TrimPrefix(m, "/host") != point {
			onlyHere = false
		}
	}
	if dev.Blocked == "mounted" && onlyHere {
		dev.Blocked = ""
	}
	if dev.Blocked != "" {
		return Result{Error: &Error{CodeDeviceBlocked, dev.Path + " cannot be mounted: " + dev.Blocked}, Devices: []Device{dev}}
	}
	cur, ok := lastCovering(ParseMounts(table), point)
	here := ok && cur.Point == point
	if here && cur.Source != dev.Path {
		return fail(CodeBusy, point+" is mounted from "+cur.Source)
	}
	if !here {
		if herr := e.preparePoint(ctx, point); herr != nil {
			return Result{Error: herr}
		}
	}
	if herr := e.writeUnit(point, dev.FSUUID); herr != nil {
		return Result{Error: herr}
	}
	if !here {
		if _, err := e.host(ctx, "mount", "-t", "xfs", "-o", "prjquota", "UUID="+dev.FSUUID, point); err != nil {
			return fail(CodeIO, err.Error())
		}
		table, err := os.ReadFile(e.mountinfo())
		if err != nil {
			return fail(CodeIO, "read the mount table: "+err.Error())
		}
		if cur, ok := lastCovering(ParseMounts(table), point); !ok || cur.Point != point || cur.FSType != "xfs" {
			return fail(CodeIO, point+" is not mounted after mount")
		}
	}
	unit := unitName(point)
	for _, args := range [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", unit}} {
		if _, err := e.host(ctx, args...); err != nil {
			return fail(CodeIO, "enable the mount unit: "+err.Error())
		}
	}
	return Result{Disk: &Fact{Path: point, FSType: "xfs", Mounted: true, Device: dev.ID, DeviceBytes: dev.Bytes, FSUUID: dev.FSUUID}}
}

// host runs a command in the host's mount namespace.
func (e Env) host(ctx context.Context, args ...string) ([]byte, error) {
	return e.exec(ctx, "nsenter", append([]string{"-t", "1", "-m", "--"}, args...)...)
}

// preparePoint makes the empty mount point, one real folder at a time (a
// symlink on the way is refused), and makes it immutable so nothing writes
// to the bare folder when the disk is not mounted.
func (e Env) preparePoint(ctx context.Context, point string) *Error {
	p := ""
	for _, c := range strings.Split(strings.TrimPrefix(point, "/"), "/") {
		p += "/" + c
		canon, err := resolveUnder(e.Root, p)
		if notThere(err) {
			if err := os.Mkdir(joinRoot(e.Root, p), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return &Error{CodeIO, err.Error()}
			}
			canon, err = resolveUnder(e.Root, p)
		}
		if err != nil {
			return &Error{CodeIO, err.Error()}
		}
		if canon != p {
			return &Error{CodeBadName, p + " is a symlink"}
		}
	}
	ents, err := os.ReadDir(joinRoot(e.Root, point))
	if err != nil {
		return &Error{CodeIO, err.Error()}
	}
	if len(ents) > 0 {
		return &Error{CodeNotEmpty, point + " is not empty"}
	}
	if _, err := e.exec(ctx, "chattr", "+i", joinRoot(e.Root, point)); err != nil {
		return &Error{CodeIO, err.Error()}
	}
	return nil
}

// unitName is the systemd mount unit for a path (systemd-escape -p --suffix=mount).
func unitName(point string) string {
	var b strings.Builder
	for _, c := range []byte(strings.Trim(point, "/")) {
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == ':', c == '.':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String() + ".mount"
}

// unitText mounts by filesystem uuid with project quotas, before Docker. nofail
// and WantedBy (not RequiredBy) keep the machine booting when the device is gone.
func unitText(point, uuid string) string {
	return "# managed by stackr\n[Unit]\nDescription=stackr disk " + path.Base(point) + "\nBefore=docker.service\n\n" +
		"[Mount]\nWhat=UUID=" + uuid + "\nWhere=" + point + "\nType=xfs\nOptions=prjquota,nofail\n\n" +
		"[Install]\nWantedBy=local-fs.target\n"
}

// writeUnit puts the unit in /etc/systemd/system of the host: a temp file,
// synced, then linked in under its name, which fails when one is there. An
// existing unit is accepted only when it is exactly the one this disk would
// get (marker, uuid, mount point); a foreign unit is never overwritten.
func (e Env) writeUnit(point, uuid string) *Error {
	const dir = "/etc/systemd/system"
	canon, err := resolveUnder(e.Root, dir)
	if err != nil {
		return &Error{CodeIO, "no " + dir + " on this host: " + err.Error()}
	}
	root, err := os.OpenRoot(joinRoot(e.Root, canon))
	if err != nil {
		return &Error{CodeIO, err.Error()}
	}
	defer func() { _ = root.Close() }()
	name := unitName(point)
	foreign := func() *Error {
		return &Error{CodeBusy, name + " already exists and is not the stackr mount unit of this disk"}
	}
	if have, err := root.ReadFile(name); err == nil {
		if string(have) == unitText(point, uuid) {
			return nil
		}
		return foreign()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return &Error{CodeIO, err.Error()}
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := ".stackr-unit-" + hex.EncodeToString(rnd[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return &Error{CodeIO, err.Error()}
	}
	_, err = f.WriteString(unitText(point, uuid))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Link(tmp, name)
	}
	_ = root.Remove(tmp)
	if errors.Is(err, fs.ErrExist) {
		return foreign()
	}
	if err != nil {
		return &Error{CodeIO, "write the mount unit: " + err.Error()}
	}
	return nil
}

// deviceBehind is the block device the xfs mount at point sits on.
func (e Env) deviceBehind(ctx context.Context, point string) (Device, *Error) {
	table, err := os.ReadFile(e.mountinfo())
	if err != nil {
		return Device{}, &Error{CodeIO, "read the mount table: " + err.Error()}
	}
	m, ok := lastCovering(ParseMounts(table), point)
	if !ok || m.Point != point {
		return Device{}, &Error{CodeDiskMissing, point + " is not mounted"}
	}
	nodes, err := e.lsblk(ctx)
	if err != nil {
		return Device{}, &Error{CodeIO, "list the devices: " + err.Error()}
	}
	byID := byIDLinks(e.Root)
	var dev Device
	var found bool
	for _, n := range nodes {
		n.all(func(c lsNode) {
			if c.Path == m.Source {
				dev, found = c.device(byID), true
			}
		})
	}
	if !found {
		return Device{}, &Error{CodeIO, "no device " + m.Source}
	}
	return dev, nil
}

// decorate adds the device facts to a disk result. Best effort: a failing
// lsblk leaves them out.
func (e Env) decorate(ctx context.Context, res Result, point string) Result {
	if res.Disk == nil {
		return res
	}
	if d, herr := e.deviceBehind(ctx, point); herr == nil {
		res.Disk.Device, res.Disk.DeviceBytes, res.Disk.FSUUID = d.ID, d.Bytes, d.FSUUID
	}
	return res
}

// xfsGrow grows the filesystem to its device. The device must not be smaller
// than the size recorded when the disk was added.
func xfsGrow(ctx context.Context, e Env, r Request) Result {
	t, s, herr := e.verified(r)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	if t.readOnly() {
		return fail(CodeReadOnly, r.Mount+" is mounted read-only")
	}
	dev, herr := e.deviceBehind(ctx, r.Mount)
	if herr != nil {
		return Result{Error: herr}
	}
	if r.FSUUID == "" || dev.FSUUID != r.FSUUID {
		return fail(CodeDeviceChanged, "the filesystem behind "+r.Mount+" is not the one recorded for this disk")
	}
	if r.DeviceBytes > 0 && dev.Bytes < r.DeviceBytes {
		return fail(CodeDeviceSmaller, dev.Path+" is smaller than before")
	}
	if _, err := e.exec(ctx, "xfs_growfs", t.root.Name()); err != nil {
		return fail(CodeIO, err.Error())
	}
	f := t.fact(s, false)
	f.Device, f.DeviceBytes, f.FSUUID = dev.ID, dev.Bytes, dev.FSUUID
	return Result{Disk: f}
}

// quota runs one xfs_quota command on the mount.
func (e Env) quota(ctx context.Context, mount, cmd string) ([]byte, error) {
	return e.exec(ctx, "xfs_quota", "-x", "-c", cmd, joinRoot(e.Root, mount))
}

// usedByProject reads xfs_quota's project block report: project to bytes.
func (e Env) usedByProject(ctx context.Context, mount string) (map[uint32]int64, error) {
	out, err := e.quota(ctx, mount, "report -p -b -N")
	if err != nil {
		return nil, err
	}
	return parseReport(out), nil
}

// parseReport reads "report -p -b -N" lines: "#42  1048576  0  102400  00 [--------]".
// The project is "#42" (or "42"); used is in KiB. Lines it cannot read are skipped.
func parseReport(out []byte) map[uint32]int64 {
	m := map[uint32]int64{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		p, err := strconv.ParseUint(strings.TrimPrefix(f[0], "#"), 10, 32)
		if err != nil {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		m[uint32(p)] = kb * 1024
	}
	return m
}

func quotaFor(r Request, n string) (Quota, *Error) {
	q := r.Quotas[n]
	if q.Project == 0 || q.Cap < 0 {
		return q, &Error{CodeBadName, "no project for " + n}
	}
	return q, nil
}

// xfsFolder makes the folders like path does, puts each into its project
// (number and inherit flag, so new files follow) and sets its hard cap. A cap
// under what the folder already holds is refused before any cap changes. The
// cap is written every time, 0 included, so a project number reused after a
// delete never keeps the old folder's limit.
func xfsFolder(ctx context.Context, e Env, r Request) Result {
	if herr := checkNames(r.Names); herr != nil {
		return Result{Error: herr}
	}
	for _, n := range r.Names {
		if _, herr := quotaFor(r, n); herr != nil {
			return Result{Error: herr}
		}
	}
	res := folder(e, r)
	if res.Error != nil {
		return res
	}
	dir := joinRoot(e.Root, r.Mount) + "/" + r.DiskID
	for _, n := range r.Names {
		if proj, inh, err := e.projectOf(dir + "/" + n); err == nil && inh && proj == r.Quotas[n].Project {
			continue // already in its project: no walk of the tree
		}
		if _, err := e.quota(ctx, r.Mount, fmt.Sprintf("project -s -p %s/%s %d", dir, n, r.Quotas[n].Project)); err != nil {
			return fail(CodeIO, err.Error())
		}
	}
	used, err := e.usedByProject(ctx, r.Mount)
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	for _, n := range r.Names {
		if q := r.Quotas[n]; q.Cap > 0 && used[q.Project] > q.Cap {
			return fail(CodeCapBelowUse, n+" already holds more than its cap")
		}
	}
	for _, n := range r.Names {
		q := r.Quotas[n]
		if _, err := e.quota(ctx, r.Mount, fmt.Sprintf("limit -p bhard=%s %d", capText(q.Cap), q.Project)); err != nil {
			return fail(CodeIO, err.Error())
		}
	}
	// xfs_quota can print an error and still exit 0: read every limit back.
	out, err := e.quota(ctx, r.Mount, "report -p -b -N")
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	hard := parseHard(out)
	for _, n := range r.Names {
		q := r.Quotas[n]
		want := int64(0)
		if q.Cap > 0 {
			want = (q.Cap + 1023) / 1024
		}
		if hard[q.Project] != want {
			return fail(CodeIO, fmt.Sprintf("the cap of %s was not applied (limit now %dk, wanted %dk)", n, hard[q.Project], want))
		}
	}
	return res
}

// projectOf is a folder's xfs project number and whether new files inherit it
// (FS_IOC_FSGETXATTR). An error means unknown: the caller sets it again.
func (e Env) projectOf(dir string) (uint32, bool, error) {
	if e.ProjOf != nil {
		return e.ProjOf(dir)
	}
	f, err := os.Open(dir)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }()
	var x struct {
		Flags, Extsize, Nextents, Projid, Cowextsize uint32
		Pad                                          [8]byte
	}
	const fsIocFsgetxattr, projInherit = 0x801c581f, 0x200
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocFsgetxattr, uintptr(unsafe.Pointer(&x))); errno != 0 {
		return 0, false, errno
	}
	return x.Projid, x.Flags&projInherit != 0, nil
}

// parseHard reads the hard limit (KiB, fourth column) per project from the
// same report lines parseReport reads.
func parseHard(out []byte) map[uint32]int64 {
	m := map[uint32]int64{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		p, err := strconv.ParseUint(strings.TrimPrefix(f[0], "#"), 10, 32)
		if err != nil {
			continue
		}
		if kb, err := strconv.ParseInt(f[3], 10, 64); err == nil {
			m[uint32(p)] = kb
		}
	}
	return m
}

// capText is a byte cap in the KiB xfs_quota reads (rounded up); 0 clears it.
func capText(b int64) string {
	if b <= 0 {
		return "0"
	}
	return strconv.FormatInt((b+1023)/1024, 10) + "k"
}

// xfsUsage is usage with the sizes from the project report, never a walk.
func xfsUsage(ctx context.Context, e Env, r Request) Result {
	if herr := checkNames(r.Names); herr != nil {
		return Result{Error: herr}
	}
	if r.Sizes {
		for _, n := range r.Names {
			if _, herr := quotaFor(r, n); herr != nil {
				return Result{Error: herr}
			}
		}
	}
	empty := r
	empty.Sizes = false
	res := usage(ctx, e, empty)
	if res.Error != nil || !r.Sizes {
		return res
	}
	used, err := e.usedByProject(ctx, r.Mount)
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	for n, u := range res.Usage {
		if !u.Missing && !u.Empty {
			u.Bytes = used[r.Quotas[n].Project]
			res.Usage[n] = u
		}
	}
	return res
}

// xfsUnmount takes a disk off the host: umount in the host namespace, the
// systemd unit disabled and removed, the mount point made writable again and
// removed. Each step is skipped when already done. Refused while anything
// else is mounted below the point. It never touches the device.
func xfsUnmount(ctx context.Context, e Env, r Request) Result {
	if herr := badName(r.DiskID); herr != nil {
		return Result{Error: herr}
	}
	point := MountPoint(r.DiskID)
	if r.Mount != point {
		return fail(CodeBadName, "an xfs disk is mounted at "+point)
	}
	table, err := os.ReadFile(e.mountinfo())
	if err != nil {
		return fail(CodeIO, "read the mount table: "+err.Error())
	}
	mounted := false
	for _, m := range ParseMounts(table) {
		switch {
		case m.Point == point:
			if m.FSType != "xfs" {
				return fail(CodeDiskMissing, point+" is not an xfs filesystem")
			}
			mounted = true
		case strings.HasPrefix(m.Point, point+"/"):
			return fail(CodeBusy, m.Point+" is mounted inside "+point)
		}
	}
	if mounted {
		if _, err := e.host(ctx, "umount", point); err != nil {
			return fail(CodeBusy, err.Error())
		}
		table, err := os.ReadFile(e.mountinfo())
		if err != nil {
			return fail(CodeIO, "read the mount table: "+err.Error())
		}
		for _, m := range ParseMounts(table) {
			if m.Point == point {
				return fail(CodeBusy, point+" is still mounted")
			}
		}
	}
	unit := unitName(point)
	if _, err := e.host(ctx, "systemctl", "disable", unit); err != nil {
		return fail(CodeIO, "disable the mount unit: "+err.Error())
	}
	if err := os.Remove(joinRoot(e.Root, "/etc/systemd/system/"+unit)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(CodeIO, err.Error())
	}
	if _, err := e.host(ctx, "systemctl", "daemon-reload"); err != nil {
		return fail(CodeIO, "reload systemd: "+err.Error())
	}
	dir := joinRoot(e.Root, point)
	if _, err := os.Lstat(dir); err == nil {
		if _, err := e.exec(ctx, "chattr", "-i", dir); err != nil {
			return fail(CodeIO, err.Error())
		}
		if err := os.Remove(dir); err != nil {
			return fail(CodeNotEmpty, point+" could not be removed: "+err.Error())
		}
	}
	return Result{}
}
