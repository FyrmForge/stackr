package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// xfsFake is the machine behind the xfs disk tests: one spare block device,
// sdb, that mkfs, wipefs and mount change, a quota report the test sets, and
// every command recorded. Nothing real runs.
type xfsFake struct {
	mu        sync.Mutex
	root      string
	mountinfo string
	calls     [][]string
	size      int64
	fstype    string // "", "ext4", "xfs"
	label     string
	uuid      string
	mounts    []string // mount points of sdb
	system    bool     // an extra mount of sdb elsewhere: it is in use
	failMount bool
	failLimit bool              // xfs_quota limit fails (the answer is lost, the limit may be set)
	used      map[uint32]int64  // KiB by project, what the quota report says
	hard      map[uint32]int64  // KiB hard limits set by limit commands
	proj      map[string]uint32 // folder -> project set by project -s commands
	aside     map[string]string
}

// unmount takes the disk away: its contents leave the mount point (an empty
// folder is all a bare mount point shows) until mount puts them back.
func (f *xfsFake) unmount(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.mounts {
		back := filepath.Join(f.root, p) + ".aside"
		must(t, os.Rename(filepath.Join(f.root, p), back))
		f.aside[p] = back
	}
	f.mounts = nil
	f.writeTable()
}

func (f *xfsFake) lsblk() string {
	node := func(path, typ string, kv ...any) map[string]any {
		m := map[string]any{"kname": filepath.Base(path), "path": path, "type": typ, "size": 1 << 30, "ro": false,
			"fstype": nil, "uuid": nil, "label": nil, "pttype": nil, "ptuuid": nil, "model": "QEMU", "serial": "S-" + filepath.Base(path),
			"wwn": nil, "mountpoints": []any{nil}, "maj:min": "8:16"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	sda := node("/dev/sda", "disk", "children", []any{node("/dev/sda1", "part", "fstype", "ext4", "mountpoints", []any{"/"})})
	mps := []any{nil}
	for _, m := range f.mounts {
		mps = append(mps[:0:0], append(mps[1:], m)...)
	}
	if f.system {
		mps = []any{"/mnt/other"}
	}
	sdb := node("/dev/sdb", "disk", "size", f.size, "mountpoints", mps)
	if f.fstype != "" {
		sdb["fstype"] = f.fstype
	}
	if f.label != "" {
		sdb["label"] = f.label
	}
	if f.uuid != "" {
		sdb["uuid"] = f.uuid
	}
	b, _ := json.Marshal(map[string]any{"blockdevices": []any{sda, sdb}})
	return string(b)
}

func (f *xfsFake) writeTable() {
	tbl := "25 1 4094:1 / / rw - ext4 /dev/sda1 rw\n"
	for i, p := range f.mounts {
		dev, err := hostdisk.DevString(filepath.Join(f.root, p))
		if err != nil {
			panic(err)
		}
		tbl += fmt.Sprintf("%d 25 %s / %s rw,prjquota - xfs /dev/sdb rw\n", 36+i, dev, p)
	}
	if err := os.WriteFile(f.mountinfo, []byte(tbl), 0o644); err != nil {
		panic(err)
	}
}

func (f *xfsFake) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	switch name {
	case "lsblk":
		return []byte(f.lsblk()), nil
	case "mkfs.xfs":
		f.fstype, f.label, f.uuid = "xfs", "stackr", "fs-uuid-0001"
	case "wipefs":
		f.fstype, f.label, f.uuid = "", "", ""
	case "nsenter":
		if len(args) > 5 && args[4] == "mount" {
			if f.failMount {
				return nil, errors.New("mount: wrong fs type")
			}
			point := args[len(args)-1]
			if back, ok := f.aside[point]; ok { // the disk's contents show again under its mount point
				_ = os.Remove(filepath.Join(f.root, point))
				_ = os.Rename(back, filepath.Join(f.root, point))
				delete(f.aside, point)
			}
			f.mounts = append(f.mounts, point)
			f.writeTable()
		}
	case "xfs_quota":
		if strings.HasPrefix(args[2], "report") {
			var out strings.Builder
			for p, kb := range f.used {
				fmt.Fprintf(&out, "#%d %d 0 %d 00 [--------]\n", p, kb, f.hard[p])
			}
			for p, h := range f.hard {
				if _, ok := f.used[p]; !ok {
					fmt.Fprintf(&out, "#%d 0 0 %d 00 [--------]\n", p, h)
				}
			}
			return []byte(out.String()), nil
		}
		var dir string
		if f.failLimit && strings.HasPrefix(args[2], "limit") {
			f.hard[1] = 100 * 1024 // it did go in, the answer was lost
			return nil, errors.New("xfs_quota: connection lost")
		}
		var kb string
		var p uint32
		if n, _ := fmt.Sscanf(args[2], "project -s -p %s %d", &dir, &p); n == 2 {
			f.proj[dir] = p
		}
		if n, _ := fmt.Sscanf(args[2], "limit -p bhard=%s %d", &kb, &p); n == 2 {
			h, _ := strconv.ParseInt(strings.TrimSuffix(kb, "k"), 10, 64)
			f.hard[p] = h
		}
	}
	return nil, nil
}

// ran is the commands run with this program, host-namespace ones included.
func (f *xfsFake) ran(name string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == "nsenter" && len(c) > 5 {
			c = c[5:]
		}
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

func (f *xfsFake) destructive() bool { return len(f.ran("mkfs.xfs"))+len(f.ran("wipefs")) > 0 }

// quotaCmds are the xfs_quota commands run, without the mount argument.
func (f *xfsFake) quotaCmds() []string {
	var out []string
	for _, c := range f.ran("xfs_quota") {
		out = append(out, c[3])
	}
	return out
}

type xfsTest struct {
	*diskTest
	fx *xfsFake
}

func newXFSTest(t *testing.T) *xfsTest {
	t.Helper()
	fx := &xfsFake{size: 1 << 30, used: map[uint32]int64{}, hard: map[uint32]int64{}, proj: map[string]uint32{}, aside: map[string]string{}}
	d := &diskTest{put: &putMesh{}}
	d.env = hostdisk.Env{Root: t.TempDir(), Mountinfo: filepath.Join(t.TempDir(), "mountinfo"), TempMounts: true, Exec: fx.exec,
		StatRdev: func(string) (string, error) { return "8:16", nil },
		ProjOf: func(dir string) (uint32, bool, error) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			p, ok := fx.proj[dir]
			return p, ok, nil
		}}
	fx.root, fx.mountinfo = d.env.Root, d.env.Mountinfo
	must(t, os.MkdirAll(filepath.Join(d.env.Root, "etc/systemd/system"), 0o755))
	fx.writeTable()
	d.twoNodes = newTwoNodes(t, WithDiskHost(InProcessDisk(d.env)))
	d.orch.diskNodeOf = func(store.Node) (MeshNode, bool) { return d.put, true }
	d.facts(t, worker.Facts{DisksRead: true})
	return &xfsTest{diskTest: d, fx: fx}
}

// pick is sdb as the picker lists it now.
func (x *xfsTest) pick(t *testing.T) DiskDevice {
	t.Helper()
	devs, err := x.orch.NodeDevices(context.Background(), adminP(), "w1")
	must(t, err)
	i := slices.IndexFunc(devs, func(d DiskDevice) bool { return d.Path == "/dev/sdb" })
	if i < 0 {
		t.Fatalf("no sdb in %+v", devs)
	}
	return devs[i]
}

// addDevice adds sdb, typing the node's name, and waits for the job.
func (x *xfsTest) addDevice(t *testing.T, force bool) Job {
	t.Helper()
	j, err := x.orch.AddNodeDevice(context.Background(), adminP(), "w1", x.pick(t), "sdb", force)
	return x.run(t, j, err)
}

// addXFS makes sdb the ready xfs disk of w1.
func (x *xfsTest) addXFS(t *testing.T) store.NodeDisk {
	t.Helper()
	if j := x.addDevice(t, false); j.State != job.Done {
		t.Fatalf("add device: %s %q", j.State, j.Error)
	}
	row, ok, err := x.orch.nodeDisk(context.Background(), "w1")
	must(t, err)
	if !ok || row.State != disk.StateReady || row.Kind != disk.KindXFS {
		t.Fatalf("row = %+v (has %v)", row, ok)
	}
	return row
}

func (x *xfsTest) folders(t *testing.T, row store.NodeDisk) map[string]int64 {
	t.Helper()
	fs, err := x.st.DiskFolders.ListByDisk(context.Background(), row.ID)
	must(t, err)
	out := map[string]int64{}
	for _, f := range fs {
		out[f.Name] = f.Project
	}
	return out
}

func quotaProject(dir string, p uint32) string { return fmt.Sprintf("project -s -p %s %d", dir, p) }

func TestXFSAddDeviceFormatsMountsAndStampsInThreeRuns(t *testing.T) { // DL: no -f; list, format, list, mount, add
	x := newXFSTest(t)
	ctx := context.Background()
	devs, err := x.orch.NodeDevices(ctx, adminP(), "w1")
	must(t, err)
	if len(devs) != 2 || devs[0].Blocked != "system" || devs[1].Blocked != "" || devs[1].Signature != "" {
		t.Fatalf("devices = %+v", devs)
	}
	// format_requires_typed_name: nothing is queued and nothing formatted without it
	for _, typed := range []string{"", "w1", "sdc", "/dev/sdc"} {
		if _, err := x.orch.AddNodeDevice(ctx, adminP(), "w1", devs[1], typed, false); err == nil {
			t.Errorf("typed %q accepted", typed)
		} else if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("typed %q: %v", typed, err)
		}
	}
	if x.fx.destructive() {
		t.Fatalf("formatted without the typed name: %v", x.fx.calls)
	}
	row := x.addXFS(t)
	if row.DeviceID != "/dev/sdb" || row.FSUUID != "fs-uuid-0001" || row.DeviceBytes != 1<<30 || row.MountPath != hostdisk.MountPoint(row.ID) {
		t.Errorf("row = %+v", row)
	}
	if _, err := os.Stat(filepath.Join(x.env.Root, row.MountPath, ".stackr-disk")); err != nil {
		t.Errorf("no stamp: %v", err)
	}
	if got := x.put.last(); len(got) != 1 || got[0] != (worker.DiskReg{ID: row.ID, Path: row.MountPath, Owner: "w1", Device: row.DeviceID}) {
		t.Errorf("registry pushed = %+v", got)
	}
	mk := x.fx.ran("mkfs.xfs")
	if len(mk) != 1 || strings.Join(mk[0], " ") != "mkfs.xfs -K -m reflink=1 -L stackr /dev/sdb" {
		t.Errorf("mkfs = %v", mk)
	}
	// a list before the format, and another one between the format and the mount
	var seq []string
	for _, c := range x.fx.calls {
		if c[0] == "nsenter" && len(c) > 5 {
			c = c[5:]
		}
		if slices.Contains([]string{"lsblk", "mkfs.xfs", "mount"}, c[0]) && (len(seq) == 0 || seq[len(seq)-1] != c[0] || c[0] != "lsblk") {
			seq = append(seq, c[0])
		}
	}
	if got := strings.Join(seq, " "); !strings.Contains(got, "lsblk mkfs.xfs lsblk mount") {
		t.Errorf("order of the runs: %s", got)
	}
	evs, err := x.orch.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.add_device" && e.TargetID == "w1" }) {
		t.Errorf("no disk.add_device event: %+v", evs)
	}
}

func TestXFSFormatAnywayOnlyWithForceAndNeverOnABlockedDevice(t *testing.T) { // DL
	ctx := context.Background()
	x := newXFSTest(t)
	x.fx.fstype, x.fx.label, x.fx.uuid = "ext4", "data", "u-old"
	p := x.pick(t)
	if p.Signature != "filesystem" {
		t.Fatalf("pick = %+v", p)
	}
	_, err := x.orch.AddNodeDevice(ctx, adminP(), "w1", p, "sdb", false)
	if !errors.Is(err, errs.ErrRefused) || err.Error() != disk.MsgNeedsForce || x.fx.destructive() {
		t.Fatalf("without force: %v, destructive %v", err, x.fx.destructive())
	}
	if _, err := x.orch.AddNodeDevice(ctx, adminP(), "w1", p, "w1", true); err == nil || x.fx.destructive() {
		t.Fatalf("force without the typed name: %v", err)
	}
	if j := x.addDevice(t, true); j.State != job.Done {
		t.Fatalf("format anyway: %s %q", j.State, j.Error)
	}
	if w, m := x.fx.ran("wipefs"), x.fx.ran("mkfs.xfs"); len(w) != 1 || len(m) != 1 {
		t.Errorf("wipefs %v, mkfs %v", w, m)
	}
	for _, c := range x.fx.ran("mkfs.xfs") {
		if slices.Contains(c, "-f") {
			t.Errorf("mkfs got -f: %v", c)
		}
	}

	// a device with no signature: force changes nothing (no wipefs)
	y := newXFSTest(t)
	if j := y.addDevice(t, true); j.State != job.Done || len(y.fx.ran("wipefs")) != 0 {
		t.Errorf("force on a blank device: %s %q wipefs %v", j.State, j.Error, y.fx.ran("wipefs"))
	}

	// a device in use is refused with force too, at the verb and, if the pick is stale, at the host
	z := newXFSTest(t)
	z.fx.fstype, z.fx.label, z.fx.uuid, z.fx.system = "ext4", "", "u", true
	b := z.pick(t)
	if b.Blocked != "mounted" {
		t.Fatalf("pick = %+v", b)
	}
	if _, err := z.orch.AddNodeDevice(ctx, adminP(), "w1", b, "sdb", true); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "in use") {
		t.Errorf("blocked pick: %v", err)
	}
	stale := b
	stale.Blocked, stale.Signature = "", ""
	j, err := z.orch.AddNodeDevice(ctx, adminP(), "w1", stale, "sdb", true)
	if j = z.run(t, j, err); j.State != job.Failed || z.fx.destructive() {
		t.Errorf("a stale pick of a mounted device: %s %q, destructive %v", j.State, j.Error, z.fx.destructive())
	}
}

func TestXFSDeviceChangedBetweenListAndFormatAbortsAndLeavesNoRow(t *testing.T) { // DL
	ctx := context.Background()
	x := newXFSTest(t)
	p := x.pick(t)
	x.fx.mu.Lock()
	x.fx.size = 2 << 30 // another disk is in that slot now
	x.fx.mu.Unlock()
	j, err := x.orch.AddNodeDevice(ctx, adminP(), "w1", p, "sdb", false)
	if j = x.run(t, j, err); j.State != job.Failed || !strings.Contains(j.Error, "changed since it was listed") {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if x.fx.destructive() {
		t.Errorf("formatted a changed device: %v", x.fx.calls)
	}
	if _, has, err := x.orch.nodeDisk(ctx, "w1"); err != nil || has {
		t.Errorf("a row is left: %v %v", has, err)
	}
}

func TestXFSAddDeviceResumeNeverFormatsAgain(t *testing.T) { // DL
	ctx := context.Background()
	x := newXFSTest(t)
	x.fx.failMount = true
	if j := x.addDevice(t, false); j.State != job.Failed {
		t.Fatalf("add with a failing mount: %s", j.State)
	}
	row, ok, err := x.orch.nodeDisk(ctx, "w1")
	if err != nil || !ok || row.State != disk.StateAdding || row.FSUUID != "fs-uuid-0001" {
		t.Fatalf("row after the failed mount = %+v %v %v", row, ok, err)
	}
	x.fx.failMount = false
	// the device now carries a stackr filesystem: without the resume rule this would need Format anyway
	if p := x.pick(t); p.Signature != "stackr" {
		t.Fatalf("pick = %+v", p)
	}
	if j := x.addDevice(t, false); j.State != job.Done {
		t.Fatalf("resume: %s %q", j.State, j.Error)
	}
	if n := len(x.fx.ran("mkfs.xfs")); n != 1 || len(x.fx.ran("wipefs")) != 0 {
		t.Errorf("mkfs ran %d times", n)
	}
	got, _, _ := x.orch.nodeDisk(ctx, "w1")
	if got.ID != row.ID || got.State != disk.StateReady {
		t.Errorf("resumed row = %+v, was %s", got, row.ID)
	}
}

func TestXFSGrowAndNewSpaceAndDamaged(t *testing.T) {
	ctx := context.Background()
	x := newXFSTest(t)
	row := x.addXFS(t)
	grow := func() Job {
		j, err := x.orch.GrowNodeDisk(ctx, adminP(), "w1")
		return x.run(t, j, err)
	}
	// the view reads the worker's facts: new space, a smaller device, another filesystem
	fact := func(bytes int64, fsuuid string) {
		x.facts(t, worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: row.MountPath, Mounted: true, Free: 1, Total: 1 << 30, ReadAt: time.Now(), Device: row.DeviceID, DeviceBytes: bytes, FSUUID: fsuuid}}})
	}
	fact(3<<30, row.FSUUID)
	if v, err := x.orch.NodeDisk(ctx, "w1"); err != nil || v.NewSpace != 2<<30 || v.Damaged != "" || v.Device != "/dev/sdb" {
		t.Errorf("view = %+v %v", v, err)
	}
	fact(1<<29, row.FSUUID)
	if v, _ := x.orch.NodeDisk(ctx, "w1"); v.Damaged != "smaller" || v.NewSpace != 0 {
		t.Errorf("smaller: %+v", v)
	}
	fact(1<<30, "other")
	if v, _ := x.orch.NodeDisk(ctx, "w1"); v.Damaged != "other_fs" {
		t.Errorf("other fs: %+v", v)
	}

	// grow_refused_when_device_smaller: nothing runs, the row keeps its size
	x.fx.mu.Lock()
	x.fx.size = 1 << 29
	x.fx.mu.Unlock()
	if j := grow(); j.State != job.Failed || !strings.Contains(j.Error, "smaller") || len(x.fx.ran("xfs_growfs")) != 0 {
		t.Fatalf("shrunk device: %s %q growfs %v", j.State, j.Error, x.fx.ran("xfs_growfs"))
	}
	if r, _, _ := x.orch.nodeDisk(ctx, "w1"); r.DeviceBytes != 1<<30 {
		t.Errorf("row size = %d", r.DeviceBytes)
	}

	// grow_argv: the filesystem is grown through the mount, the row records the new size
	x.fx.mu.Lock()
	x.fx.size = 3 << 30
	x.fx.mu.Unlock()
	if j := grow(); j.State != job.Done {
		t.Fatalf("grow: %s %q", j.State, j.Error)
	}
	g := x.fx.ran("xfs_growfs")
	if len(g) != 1 || len(g[0]) != 2 || g[0][1] != filepath.Join(x.env.Root, row.MountPath) {
		t.Errorf("growfs = %v", g)
	}
	if r, _, _ := x.orch.nodeDisk(ctx, "w1"); r.DeviceBytes != 3<<30 {
		t.Errorf("row size = %d", r.DeviceBytes)
	}
	evs, err := x.orch.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.grow" }) {
		t.Error("no disk.grow event")
	}
}

func TestPathDiskCannotGrowAndSavesACapThatIsNotEnforced(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	if _, err := d.orch.GrowNodeDisk(ctx, adminP(), "w1"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("grow on a path disk: %v", err)
	}
	if _, err := d.orch.AddNodeDevice(ctx, adminP(), "w1", DiskDevice{ID: "/dev/sdb", Path: "/dev/sdb"}, "sdb", false); err == nil {
		t.Error("a path disk took a second disk")
	}
	v := d.vol(t, "data")
	d.ensure(t, v)
	mb := int64(50)
	must(t, d.orch.SetVolumeCap(ctx, v.ID, &mb))
	got, err := d.orch.Volume(ctx, v.ID)
	must(t, err)
	if got.MaxSizeMB == nil || *got.MaxSizeMB != 50 {
		t.Errorf("cap = %v", got.MaxSizeMB)
	}
	if on, err := d.orch.CapEnforced(ctx, got); err != nil || on {
		t.Errorf("a path disk's cap is enforced: %v %v", on, err)
	}
	must(t, d.orch.SetVolumeCap(ctx, v.ID, nil))
	if got, _ = d.orch.Volume(ctx, v.ID); got.MaxSizeMB != nil {
		t.Errorf("cap = %v after clearing", got.MaxSizeMB)
	}
}

func TestXFSPlaceAssignsProjectsAndCapsSurviveAReplace(t *testing.T) { // DL: caps survive a re-place
	ctx := context.Background()
	x := newXFSTest(t)
	row := x.addXFS(t)
	dir := filepath.Join(x.env.Root, row.MountPath, row.ID)
	a, b, c := x.vol(t, "a"), x.vol(t, "b"), x.vol(t, "c")
	for _, v := range []store.Volume{a, b, c} {
		x.ensure(t, v)
	}
	fs := x.folders(t, row)
	if fs[a.Name] != 1 || fs[b.Name] != 2 || fs[c.Name] != 3 {
		t.Fatalf("projects = %v", fs)
	}
	cmds := x.fx.quotaCmds()
	for i, v := range []store.Volume{a, b, c} {
		if !slices.Contains(cmds, quotaProject(dir+"/"+v.Name, uint32(i+1))) || !slices.Contains(cmds, fmt.Sprintf("limit -p bhard=0 %d", i+1)) {
			t.Errorf("no project or limit for %s: %v", v.Slug, cmds)
		}
	}
	// the next project is max plus one
	must(t, x.orch.volumes.Delete(ctx, b, nil))
	d := x.vol(t, "d")
	x.ensure(t, d)
	if p := x.folders(t, row)[d.Name]; p != 4 {
		t.Errorf("project of d = %d, want 4 (a 1, c 3 are in use)", p)
	}

	// SetVolumeCap puts the cap on the folder at once
	mb := int64(300)
	must(t, x.orch.SetVolumeCap(ctx, a.ID, &mb))
	got, _ := x.orch.Volume(ctx, a.ID)
	if on, err := x.orch.CapEnforced(ctx, got); err != nil || !on {
		t.Errorf("an xfs folder's cap is not enforced: %v %v", on, err)
	}
	if cmds := x.fx.quotaCmds(); cmds[len(cmds)-2] != "limit -p bhard=307200k 1" {
		t.Errorf("limit command = %q", cmds[len(cmds)-2])
	}

	// cap_lower_than_use_refused: the host says no before anything changes, the row keeps the old cap
	x.fx.mu.Lock()
	x.fx.used[1] = 200 * 1024
	x.fx.mu.Unlock()
	low := int64(100)
	if err := x.orch.SetVolumeCap(ctx, a.ID, &low); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "more than that cap") {
		t.Fatalf("cap below use: %v", err)
	}
	if got, _ = x.orch.Volume(ctx, a.ID); got.MaxSizeMB == nil || *got.MaxSizeMB != 300 {
		t.Errorf("cap after the refusal = %v", got.MaxSizeMB)
	}
	for _, c := range x.fx.quotaCmds() {
		if c == "limit -p bhard=102400k 1" {
			t.Error("the lower cap reached the host")
		}
	}
	for _, bad := range []int64{0, -5} {
		if err := x.orch.SetVolumeCap(ctx, a.ID, &bad); err == nil {
			t.Errorf("cap %d accepted", bad)
		}
	}

	// above the largest cap: its own refusal, nothing saved
	huge := int64(1<<30 + 1)
	if err := x.orch.SetVolumeCap(ctx, a.ID, &huge); err == nil || !strings.Contains(err.Error(), "Give a size up to 1 PB.") {
		t.Errorf("cap over 1 PB = %v, want the 1 PB refusal", err)
	}

	// cap_reapplied_on_place: the folder is placed again (a deploy after the Docker object went)
	must(t, x.wFake.RemoveVolume(ctx, a.Name))
	before := len(x.fx.quotaCmds())
	x.ensure(t, a)
	again := x.fx.quotaCmds()[before:]
	if !slices.Contains(again, "limit -p bhard=307200k 1") || slices.Contains(again, quotaProject(dir+"/"+a.Name, 1)) {
		t.Errorf("re-place with the cap unchanged should put the cap back and skip the project: %v", again)
	}
	// a cap change since the last apply sets the project again
	big := int64(400)
	must(t, x.orch.SetVolumeCap(ctx, a.ID, &big))
	must(t, x.wFake.RemoveVolume(ctx, a.Name))
	before = len(x.fx.quotaCmds())
	x.ensure(t, a)
	if again = x.fx.quotaCmds()[before:]; !slices.Contains(again, "limit -p bhard=409600k 1") {
		t.Errorf("a changed cap was not applied: %v", again)
	}
	if x.folders(t, row)[a.Name] != 1 {
		t.Errorf("project changed on a re-place: %v", x.folders(t, row))
	}
}

func TestXFSUsageIsTheProjectReportNotAWalk(t *testing.T) {
	ctx := context.Background()
	x := newXFSTest(t)
	row := x.addXFS(t)
	a, b := x.vol(t, "a"), x.vol(t, "b")
	x.ensure(t, a)
	x.ensure(t, b)
	must(t, os.WriteFile(filepath.Join(x.folder(row, a.Name), "f"), []byte("x"), 0o644))
	x.fx.mu.Lock()
	x.fx.used[1], x.fx.used[2] = 2048, 9999
	x.fx.mu.Unlock()
	use, err := x.orch.diskLeaf.Usage(ctx, row, true, a.Name, b.Name, "no-row")
	must(t, err)
	if use[a.Name].Bytes != 2048*1024 || use[b.Name].Bytes != 0 || !use[b.Name].Empty {
		t.Errorf("usage = %+v", use)
	}
	if _, ok := use["no-row"]; ok {
		t.Errorf("a name with no folder row was sized: %+v", use)
	}
}

// xfs_disk_adopt: the device is matched by its filesystem uuid, the stamp owner is rewritten,
// and, like a folder disk, only a forgotten old node releases it: offline is not enough, nothing fences it.
func TestXFSAdoptMatchesByFSUUIDAndNeedsAForgottenOldNode(t *testing.T) { // DL: adopt on xfs
	ctx := context.Background()
	x := newXFSTest(t)
	row := x.addXFS(t)
	vol := x.vol(t, "data")
	x.ensure(t, vol)
	file := filepath.Join(x.folder(row, vol.Name), "DATA")
	must(t, os.WriteFile(file, []byte("precious"), 0o644))
	// the old node: a row that is offline, with the stamp and the env on it
	must(t, x.st.Nodes.Create(ctx, store.Node{ID: "old", Name: "oldbox", Slug: "oldbox", Role: "worker", Tags: "[]", RangeCIDR: "10.210.0.0/16", Status: "offline", CreatedAt: time.Now()}))
	row.NodeID = "old"
	must(t, x.st.NodeDisks.Update(ctx, row))
	b, err := os.ReadFile(filepath.Join(x.env.Root, row.MountPath, ".stackr-disk"))
	must(t, err)
	var s hostdisk.Stamp
	must(t, json.Unmarshal(b, &s))
	s.Owner = "old"
	b, _ = json.Marshal(s)
	must(t, os.WriteFile(filepath.Join(x.env.Root, row.MountPath, ".stackr-disk"), b, 0o644))
	must(t, x.st.Environments.SetNode(ctx, x.edge.ID, "old"))
	must(t, x.wFake.RemoveVolume(ctx, vol.Name))

	// X4: the old node is offline, not forgotten: not a candidate, the plan and the adopt refuse
	x.fx.unmount(t)
	plan, err := x.orch.PlanDiskAdopt(ctx, "w1", "")
	must(t, err)
	if len(plan.Candidates) != 0 {
		t.Fatalf("an offline node's xfs disk is a candidate: %+v", plan.Candidates)
	}
	if plan, err = x.orch.PlanDiskAdopt(ctx, "w1", row.MountPath); err != nil || !strings.Contains(plan.Refusal, "offline but not forgotten") {
		t.Fatalf("plan with the old node offline = %+v %v", plan, err)
	}
	if _, err := x.orch.AdoptDisk(ctx, adminP(), "w1", row.ID, row.MountPath, "w1"); !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("adopt with the old node offline: %v", err)
	}
	if len(x.fx.ran("mount")) != 1 { // the add's mount only
		t.Fatalf("mounted for a refused adopt: %v", x.fx.ran("mount"))
	}
	// the node is forgotten (no row): the disk is listed among the candidates
	must(t, x.st.Nodes.Delete(ctx, "old"))
	plan, err = x.orch.PlanDiskAdopt(ctx, "w1", "")
	must(t, err)
	if len(plan.Candidates) != 1 || plan.Candidates[0].DiskID != row.ID || plan.Candidates[0].Path != row.MountPath {
		t.Fatalf("candidates = %+v", plan.Candidates)
	}
	// the disk is not mounted on w1 yet: the plan finds it by its filesystem uuid
	plan, err = x.orch.PlanDiskAdopt(ctx, "w1", row.MountPath)
	must(t, err)
	if plan.Refusal != "" || plan.DiskID != row.ID {
		t.Fatalf("plan = %+v", plan)
	}
	// a node that does not list the device cannot adopt
	x.fx.mu.Lock()
	x.fx.uuid = "fs-other"
	x.fx.mu.Unlock()
	if _, err := x.orch.AdoptDisk(ctx, adminP(), "w1", row.ID, row.MountPath, "w1"); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "device is not on this node") {
		t.Errorf("adopt without the device: %v", err)
	}
	x.fx.mu.Lock()
	x.fx.uuid = "fs-uuid-0001"
	x.fx.mu.Unlock()
	// a path other than the disk's mount point is refused
	if _, err := x.orch.AdoptDisk(ctx, adminP(), "w1", row.ID, "/disk", "w1"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("adopt at /disk: %v", err)
	}
	j, err := x.orch.AdoptDisk(ctx, adminP(), "w1", row.ID, row.MountPath, "w1")
	if j = x.run(t, j, err); j.State != job.Done {
		t.Fatalf("adopt = %s %q", j.State, j.Error)
	}
	got, _, _ := x.orch.nodeDisk(ctx, "w1")
	if got.ID != row.ID || got.State != disk.StateReady || got.Kind != disk.KindXFS {
		t.Errorf("row = %+v", got)
	}
	if e, _ := x.st.Environments.Get(ctx, x.edge.ID); e.NodeID != "w1" {
		t.Errorf("env on %s", e.NodeID)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "precious" {
		t.Errorf("data = %q %v", b, err)
	}
	if len(x.fx.ran("mount")) == 0 {
		t.Error("the device was not mounted on the new node")
	}
	sb, _ := os.ReadFile(filepath.Join(x.env.Root, row.MountPath, ".stackr-disk"))
	if !strings.Contains(string(sb), `"owner":"w1"`) {
		t.Errorf("stamp owner not rewritten: %s", sb)
	}
}

func TestXFSForgetOfAMissingDiskKeepsFoldersAndDeletesRows(t *testing.T) { // DL: forget on xfs
	ctx := context.Background()
	x := newXFSTest(t)
	row := x.addXFS(t)
	v := x.vol(t, "data")
	x.ensure(t, v)
	file := filepath.Join(x.folder(row, v.Name), "DATA")
	must(t, os.WriteFile(file, []byte("precious"), 0o644))
	setState(t, x.diskTest, row, disk.StateMissing)
	j, err := x.orch.ForgetNodeDisk(ctx, adminP(), row.ID, "w1")
	if j = x.run(t, j, err); j.State != job.Done {
		t.Fatalf("forget = %s %q", j.State, j.Error)
	}
	if _, err := x.st.NodeDisks.Get(ctx, row.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("row still there: %v", err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "precious" {
		t.Errorf("folder data = %q %v", b, err)
	}
}

// xfsMoveRig is a plain volume "data" with a cap of 500 MB, a tile that mounts
// it, then a ready xfs disk on w1.
func xfsMoveRig(t *testing.T) (*xfsTest, store.Volume, store.NodeDisk) {
	t.Helper()
	ctx := context.Background()
	x := newXFSTest(t)
	v := x.vol(t, "data")
	x.ensure(t, v) // a plain volume: no disk yet
	row := x.addXFS(t)
	x.diskTile(t, "db", "data:/data")
	x.wFake.Volumes = []docker.VolumeInfo{{Name: v.Name, SizeBytes: 100}}
	x.wFake.Containers = []docker.Container{{ID: "c-db", Name: "db", State: "running",
		Labels: map[string]string{tile.LabelTile: "none", tile.LabelRole: "replica"}}}
	x.facts(t, worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: row.MountPath, Mounted: true, Free: 1 << 30, Total: 1 << 40}}})
	x.wFake.RunID = "n-db" // a deploy makes a container with a new id
	x.wFake.Details = map[string]docker.Detail{"n-db": {Running: true, Networks: map[string]string{x.edge.Network: "10.0.0.2"}}}
	x.wFake.OnUntar = func(name string) {
		_ = os.WriteFile(filepath.Join(x.folder(row, name), "file"), []byte("data"), 0o644)
	}
	mb := int64(500)
	v.MaxSizeMB = &mb // saved while the volume was not enforced
	must(t, x.st.Volumes.Update(ctx, v))
	return x, v, row
}

func TestXFSMoveToDiskPlacesTheVolumeInAProject(t *testing.T) {
	ctx := context.Background()
	x, v, row := xfsMoveRig(t)
	j, err := x.orch.MoveVolumeToDisk(ctx, adminP(), v.ID)
	if j = x.run(t, j, err); j.State != job.Done {
		t.Fatalf("move: %s %q", j.State, j.Error)
	}
	target := moveTarget(v.ID)
	if p := x.folders(t, row)[target]; p != 1 {
		t.Fatalf("folders = %v", x.folders(t, row))
	}
	dir := filepath.Join(x.env.Root, row.MountPath, row.ID, target)
	cmds := x.fx.quotaCmds()
	if !slices.Contains(cmds, quotaProject(dir, 1)) || !slices.Contains(cmds, "limit -p bhard=512000k 1") {
		t.Errorf("no project and cap for the moved folder: %v", cmds)
	}
}

// A cap saved earlier (the volume was not enforced then) under what the volume
// holds: the copy and the swap are done when the deploy re-places the folder
// and the host refuses. The data is on the disk and the old copy is kept as an
// orphan; the person raises the cap and deploys again. (A pre-check at enqueue
// in volumemove.go would refuse it before the copy.)
func TestXFSMoveWithACapBelowTheDataFailsTheDeployAndKeepsTheData(t *testing.T) {
	ctx := context.Background()
	x, v, row := xfsMoveRig(t)
	x.fx.mu.Lock()
	x.fx.used[1] = 900 * 1024 // 900 MB in project 1, the first folder of the disk
	x.fx.mu.Unlock()
	j, err := x.orch.MoveVolumeToDisk(ctx, adminP(), v.ID)
	if j = x.run(t, j, err); j.State != job.Failed || !strings.Contains(j.Error, "more than that cap") {
		t.Fatalf("move = %s %q", j.State, j.Error)
	}
	target := moveTarget(v.ID)
	if b, err := os.ReadFile(filepath.Join(x.folder(row, target), "file")); err != nil || string(b) != "data" {
		t.Errorf("the copy is not on the disk: %q %v", b, err)
	}
	if got, _ := x.orch.Volume(ctx, v.ID); got.Name != target {
		t.Errorf("row names %q", got.Name)
	}
	if vs, _ := x.st.Volumes.ListOrphaned(ctx); !slices.ContainsFunc(vs, func(o store.Volume) bool { return o.Name == v.Name }) {
		t.Error("the old copy has no orphan row")
	}
	// raising the cap puts it right
	big := int64(2000)
	must(t, x.orch.SetVolumeCap(ctx, v.ID, &big))
}

// X5: an I/O failure while the limit goes on may have applied the lower limit, so the row keeps the
// cap and the panel says it is not applied yet; the next place puts it on. A refusal rolls back.
func TestXFSCapKeptPendingWhenTheHostAnswerIsLost(t *testing.T) { // DL
	ctx := context.Background()
	x := newXFSTest(t)
	x.addXFS(t)
	a := x.vol(t, "a")
	x.ensure(t, a)
	mb := int64(300)
	must(t, x.orch.SetVolumeCap(ctx, a.ID, &mb))
	x.fx.mu.Lock()
	x.fx.failLimit = true
	x.fx.mu.Unlock()
	low := int64(100)
	err := x.orch.SetVolumeCap(ctx, a.ID, &low)
	if err == nil || !strings.Contains(err.Error(), "did not confirm") {
		t.Fatalf("lost answer: %v", err)
	}
	got, _ := x.orch.Volume(ctx, a.ID)
	if got.MaxSizeMB == nil || *got.MaxSizeMB != 100 {
		t.Errorf("the row was rolled back to %v although the host may have the lower limit", got.MaxSizeMB)
	}
	if !x.orch.CapPending(got) {
		t.Error("the cap is not marked pending")
	}
	// the next place puts the cap on again and clears the mark
	x.fx.mu.Lock()
	x.fx.failLimit = false
	x.fx.mu.Unlock()
	must(t, x.wFake.RemoveVolume(ctx, a.Name))
	before := len(x.fx.quotaCmds())
	x.ensure(t, a)
	if again := x.fx.quotaCmds()[before:]; !slices.Contains(again, "limit -p bhard=102400k 1") {
		t.Errorf("place did not put the cap back: %v", again)
	}
	if x.orch.CapPending(got) {
		t.Error("still pending after a place")
	}
	// a refusal before any change is not ambiguous: it rolls back and marks nothing
	x.fx.mu.Lock()
	x.fx.used[1] = 200 * 1024
	x.fx.mu.Unlock()
	tiny := int64(50)
	if err := x.orch.SetVolumeCap(ctx, a.ID, &tiny); !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("cap below use: %v", err)
	}
	if got, _ = x.orch.Volume(ctx, a.ID); *got.MaxSizeMB != 100 || x.orch.CapPending(got) {
		t.Errorf("after a refusal: cap %v pending %v", *got.MaxSizeMB, x.orch.CapPending(got))
	}
}
