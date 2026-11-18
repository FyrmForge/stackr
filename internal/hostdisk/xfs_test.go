package hostdisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExec records every command and answers by name; nothing real runs.
type fakeExec struct {
	calls [][]string
	out   func(argv []string) ([]byte, error)
}

func (f *fakeExec) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	f.calls = append(f.calls, argv)
	return f.out(argv)
}

// ran is the recorded commands whose program (after nsenter -t 1 -m --) is name.
func (f *fakeExec) ran(name string) [][]string {
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

func (f *fakeExec) destructive() bool {
	return len(f.ran("mkfs.xfs"))+len(f.ran("wipefs")) > 0
}

func dev(path, typ string, kv ...any) map[string]any {
	mm := "8:0"
	if path == "/dev/sdb" {
		mm = "8:16"
	}
	m := map[string]any{"kname": filepath.Base(path), "path": path, "maj:min": mm, "type": typ, "size": 1 << 30, "ro": false,
		"fstype": nil, "uuid": nil, "label": nil, "pttype": nil, "ptuuid": nil, "model": "QEMU", "serial": "S-" + filepath.Base(path),
		"wwn": nil, "mountpoints": []any{nil}}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func lsJSON(nodes ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"blockdevices": nodes})
	return string(b)
}

var sysDisk = dev("/dev/sda", "disk", "children", []any{dev("/dev/sda1", "part", "fstype", "ext4", "mountpoints", []any{"/"})})

type xrig struct {
	t     *testing.T
	env   Env
	fx    *fakeExec
	lsblk string
	real  string // the real folder behind the mount point
	rdev  string // what stat says the device node is
	table string // extra mountinfo lines
}

func newX(t *testing.T, devs ...map[string]any) *xrig {
	t.Helper()
	root := t.TempDir()
	x := &xrig{t: t, fx: &fakeExec{}, real: filepath.Join(root, "var/lib/stackr-disks/d1"), rdev: "8:16"}
	if err := os.MkdirAll(x.real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	x.env = Env{Root: root, Mountinfo: filepath.Join(t.TempDir(), "mountinfo"), TempMounts: true, Exec: x.fx.exec,
		StatRdev: func(p string) (string, error) {
			x.fx.calls = append(x.fx.calls, []string{"stat", p})
			return x.rdev, nil
		},
		ProjOf: func(string) (uint32, bool, error) { return 0, false, errors.New("no ioctl in tests") }}
	x.lsblk = lsJSON(append([]map[string]any{sysDisk}, devs...)...)
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "lsblk" {
			return []byte(x.lsblk), nil
		}
		return nil, nil
	}
	x.setTable("")
	return x
}

func (x *xrig) setTable(extra string) {
	x.t.Helper()
	dev, err := DevString(x.real)
	if err != nil {
		x.t.Fatal(err)
	}
	x.table = extra
	s := strings.ReplaceAll(rootLine+extra, " 8:17 ", " "+dev+" ")
	if err := os.WriteFile(x.env.Mountinfo, []byte(s), 0o644); err != nil {
		x.t.Fatal(err)
	}
}

const xfsLine = "36 25 8:17 / /var/lib/stackr-disks/d1 rw,prjquota - xfs /dev/sdb rw\n"

func (x *xrig) run(op string, mod func(*Request)) Result {
	req := Request{Op: op, Kind: "xfs", Mount: MountPoint("d1"), DiskID: "d1", Owner: "n1", Install: "i1"}
	if mod != nil {
		mod(&req)
	}
	return Run(context.Background(), x.env, req)
}

// listed is the device sdb as the list shows it.
func (x *xrig) listed() Device {
	x.t.Helper()
	res := x.run("list", nil)
	wantCode(x.t, res, "")
	d, ok := find(res.Devices, "/dev/sdb")
	if !ok {
		x.t.Fatalf("no sdb in %+v", res.Devices)
	}
	return d
}

func (x *xrig) format(force bool) Result {
	d := x.listed()
	return x.run("format", func(q *Request) { q.Device, q.Expect, q.Force = d.ID, &d, force })
}

func plain() map[string]any { return dev("/dev/sdb", "disk") }

func TestListShowsWholeDisksOnly(t *testing.T) {
	x := newX(t, plain(), dev("/dev/loop0", "loop"), dev("/dev/sr0", "rom"))
	res := x.run("list", nil)
	wantCode(t, res, "")
	if len(res.Devices) != 2 || res.Devices[0].Path != "/dev/sda" || res.Devices[1].Path != "/dev/sdb" {
		t.Fatalf("%+v", res.Devices)
	}
	if d := res.Devices[1]; d.Blocked != "" || d.Signature != "" || d.Bytes != 1<<30 || d.Serial != "S-sdb" {
		t.Fatalf("%+v", d)
	}
	if res.Devices[0].Blocked != "system" {
		t.Fatalf("system disk is %q", res.Devices[0].Blocked)
	}
}

func TestListReadsOlderLsblkStringTypes(t *testing.T) {
	x := newX(t)
	x.lsblk = `{"blockdevices":[{"kname":"sdb","path":"/dev/sdb","type":"disk","size":"2048","ro":"0","fstype":null,"mountpoints":[null]}]}`
	if d := x.listed(); d.Bytes != 2048 || d.Blocked != "" {
		t.Fatalf("%+v", d)
	}
}

func TestListNamesADeviceByItsStableLink(t *testing.T) {
	x := newX(t, plain())
	dir := filepath.Join(x.env.Root, "dev/disk/by-id")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"wwn-0x5000", "scsi-0QEMU_b", "scsi-0QEMU_a"} {
		_ = os.Symlink("../../sdb", filepath.Join(dir, l))
	}
	_ = os.Symlink("../../sdb1", filepath.Join(dir, "scsi-0QEMU_a-part1"))
	if d := x.listed(); d.ID != "/dev/disk/by-id/scsi-0QEMU_a" {
		t.Fatalf("id %q", d.ID)
	}
}

// the guards that no Force overrides
var blockedCases = []struct {
	name, want string
	node       map[string]any
	table      string
	holders    bool
}{
	{name: "mounted", want: "mounted", node: dev("/dev/sdb", "disk", "fstype", "ext4", "mountpoints", []any{"/mnt/x"})},
	{name: "partition mounted", want: "mounted", node: dev("/dev/sdb", "disk", "children", []any{dev("/dev/sdb1", "part", "fstype", "ext4", "mountpoints", []any{"/data"})})},
	{name: "mounted by the table only", want: "mounted", node: plain(), table: "40 25 8:99 / /mnt/y rw - ext4 /dev/sdb rw\n"},
	{name: "partitions", want: "partitions", node: dev("/dev/sdb", "disk", "pttype", "gpt", "children", []any{dev("/dev/sdb1", "part")})},
	{name: "root device", want: "system", node: dev("/dev/sdb", "disk", "fstype", "ext4", "mountpoints", []any{"/"})},
	{name: "docker root device", want: "system", node: plain(), table: "40 25 8:99 / /var/lib/docker rw - ext4 /dev/sdb rw\n"},
	{name: "lvm pv", want: "lvm", node: dev("/dev/sdb", "disk", "fstype", "LVM2_member")},
	{name: "md member", want: "md", node: dev("/dev/sdb", "disk", "fstype", "linux_raid_member")},
	{name: "luks", want: "luks", node: dev("/dev/sdb", "disk", "fstype", "crypto_LUKS")},
	{name: "active swap", want: "swap", node: dev("/dev/sdb", "disk", "fstype", "swap", "mountpoints", []any{"[SWAP]"})},
	{name: "swap signature", want: "swap", node: dev("/dev/sdb", "disk", "fstype", "swap")},
	{name: "zfs member", want: "zfs", node: dev("/dev/sdb", "disk", "fstype", "zfs_member")},
	{name: "holder in lsblk", want: "holders", node: dev("/dev/sdb", "disk", "children", []any{dev("/dev/mapper/x", "lvm")})},
	{name: "holder in sysfs", want: "holders", node: plain(), holders: true},
	{name: "read-only", want: "readonly", node: dev("/dev/sdb", "disk", "ro", true)},
}

func TestFormatGuardsRefuseWithNoOverride(t *testing.T) { // DL: mounted, partitions, root/docker root, lvm, md, luks, swap, zfs, holders
	for _, c := range blockedCases {
		for _, force := range []bool{false, true} {
			t.Run(c.name, func(t *testing.T) {
				x := newX(t, c.node)
				if c.table != "" {
					x.setTable(c.table)
				}
				if c.holders {
					if err := os.MkdirAll(filepath.Join(x.env.Root, "sys/block/sdb/holders/dm-0"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				d := x.listed()
				if d.Blocked != c.want {
					t.Fatalf("listed blocked %q, want %q", d.Blocked, c.want)
				}
				res := x.run("format", func(q *Request) { q.Device, q.Expect, q.Force = d.ID, &d, force })
				wantCode(t, res, CodeDeviceBlocked)
				if len(res.Devices) != 1 || res.Devices[0].Blocked != c.want {
					t.Fatalf("result devices %+v", res.Devices)
				}
				if x.fx.destructive() {
					t.Fatalf("a destructive command ran: %v", x.fx.calls)
				}
			})
		}
	}
}

func TestFormatSignatureNeedsForce(t *testing.T) { // DL
	for _, c := range []struct {
		name, sig string
		node      map[string]any
	}{
		{"filesystem", "filesystem", dev("/dev/sdb", "disk", "fstype", "ext4", "uuid", "u-1")},
		{"stackr stamp", "stackr", dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-1")},
		{"partition table", "table", dev("/dev/sdb", "disk", "pttype", "gpt")},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newX(t, c.node)
			if d := x.listed(); d.Signature != c.sig {
				t.Fatalf("signature %q", d.Signature)
			}
			wantCode(t, x.format(false), CodeNeedsForce)
			if x.fx.destructive() {
				t.Fatalf("ran without force: %v", x.fx.calls)
			}
			// with Force the signature is wiped first, then mkfs
			x.fx.out = func(argv []string) ([]byte, error) {
				if argv[0] == "lsblk" {
					if len(x.fx.ran("mkfs.xfs")) > 0 {
						return []byte(lsJSON(sysDisk, dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-2"))), nil
					}
					return []byte(x.lsblk), nil
				}
				return nil, nil
			}
			res := x.format(true)
			wantCode(t, res, "")
			if w, m := x.fx.ran("wipefs"), x.fx.ran("mkfs.xfs"); len(w) != 1 || len(m) != 1 || strings.Join(w[0], " ") != "wipefs -a /dev/sdb" {
				t.Fatalf("calls %v", x.fx.calls)
			}
			if res.Devices[0].FSUUID != "u-2" {
				t.Fatalf("%+v", res.Devices)
			}
		})
	}
}

// formatted is the answer of lsblk once mkfs ran.
func formatted(x *xrig) {
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "lsblk" {
			if len(x.fx.ran("mkfs.xfs")) > 0 {
				return []byte(lsJSON(sysDisk, dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-new"))), nil
			}
			return []byte(x.lsblk), nil
		}
		return nil, nil
	}
}

func TestMkfsArgvHasReflinkAndNoPartitionTable(t *testing.T) {
	x := newX(t, plain())
	formatted(x)
	res := x.format(false)
	wantCode(t, res, "")
	m := x.fx.ran("mkfs.xfs")
	if len(m) != 1 || strings.Join(m[0], " ") != "mkfs.xfs -K -m reflink=1 -L stackr /dev/sdb" {
		t.Fatalf("%v", m)
	}
	if len(x.fx.ran("wipefs")) != 0 || len(x.fx.ran("parted"))+len(x.fx.ran("sfdisk"))+len(x.fx.ran("sgdisk")) != 0 {
		t.Fatalf("%v", x.fx.calls)
	}
	if d := res.Devices[0]; d.FSUUID != "u-new" || d.FSType != "xfs" {
		t.Fatalf("%+v", d)
	}
}

func TestMkfsNeverGetsDashF(t *testing.T) { // DL
	check := func(x *xrig) {
		for _, c := range x.fx.calls {
			for _, a := range c[1:] {
				if a == "-f" || strings.HasPrefix(a, "-f") && !strings.HasPrefix(a, "--") {
					t.Fatalf("-f in %v", c)
				}
			}
		}
	}
	x := newX(t, plain())
	formatted(x)
	wantCode(t, x.format(false), "")
	check(x)
	x = newX(t, dev("/dev/sdb", "disk", "fstype", "ext4", "uuid", "u-1"))
	formatted(x)
	wantCode(t, x.format(true), "")
	check(x)
}

func TestFormatAbortsWhenTheDeviceChangedSinceTheList(t *testing.T) { // DL
	for _, c := range []struct {
		name string
		now  map[string]any
	}{
		{"filesystem appeared", dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-9")},
		{"size changed", dev("/dev/sdb", "disk", "size", 2<<30)},
		{"another disk in the slot", dev("/dev/sdb", "disk", "serial", "other")},
		{"partition table appeared", dev("/dev/sdb", "disk", "pttype", "gpt", "ptuuid", "p-1")},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newX(t, plain())
			d := x.listed()
			x.lsblk = lsJSON(sysDisk, c.now)
			for _, force := range []bool{false, true} {
				res := x.run("format", func(q *Request) { q.Device, q.Expect, q.Force = d.ID, &d, force })
				wantCode(t, res, CodeDeviceChanged)
			}
			if x.fx.destructive() {
				t.Fatalf("formatted a changed device: %v", x.fx.calls)
			}
		})
	}
}

func TestFormatNeedsAnIdentityAndARealDevice(t *testing.T) { // DL
	x := newX(t, plain())
	d := x.listed()
	wantCode(t, x.run("format", func(q *Request) { q.Device = d.ID }), CodeDeviceChanged)
	wantCode(t, x.run("format", func(q *Request) { q.Device, q.Expect = "/dev/sdz", &d }), CodeNoDevice)
	wantCode(t, x.run("format", func(q *Request) { q.Expect = &d }), CodeNoDevice)
	if x.fx.destructive() {
		t.Fatal("formatted")
	}
}

func TestFormatFailureIsIONotSuccess(t *testing.T) {
	x := newX(t, plain())
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "lsblk" {
			return []byte(x.lsblk), nil
		}
		return nil, errors.New("mkfs.xfs: existing filesystem")
	}
	wantCode(t, x.format(false), CodeIO)
}

func TestMountUnitText(t *testing.T) {
	want := "# managed by stackr\n[Unit]\nDescription=stackr disk d1\nBefore=docker.service\n\n" +
		"[Mount]\nWhat=UUID=u-new-1234\nWhere=/var/lib/stackr-disks/d1\nType=xfs\nOptions=prjquota,nofail\n\n" +
		"[Install]\nWantedBy=local-fs.target\n"
	if got := unitText(MountPoint("d1"), "u-new-1234"); got != want {
		t.Fatalf("%q", got)
	}
	if n := unitName(MountPoint("d1")); n != `var-lib-stackr\x2ddisks-d1.mount` {
		t.Fatal(n)
	}
	if n := unitName(MountPoint("a-b_c.d")); n != `var-lib-stackr\x2ddisks-a\x2db_c.d.mount` {
		t.Fatal(n)
	}
}

// storedDisk is a formatted stackr xfs device waiting to be mounted.
func storedDisk() map[string]any {
	return dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-new-1234")
}

// mounting: the fake host puts the mount into the table when asked.
func mounting(x *xrig) {
	inner := x.fx.out
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "nsenter" && argv[5] == "mount" {
			x.setTable(xfsLine)
		}
		return inner(argv)
	}
}

func (x *xrig) mount() Result {
	d := x.listed()
	return x.run("mount", func(q *Request) { q.Device, q.Expect = d.ID, &d })
}

func TestMountRunsInTheHostNamespaceAndWritesTheUnit(t *testing.T) {
	x := newX(t, storedDisk())
	mounting(x)
	res := x.mount()
	wantCode(t, res, "")
	if f := res.Disk; f == nil || !f.Mounted || f.FSUUID != "u-new-1234" || f.Device != "/dev/sdb" || f.DeviceBytes != 1<<30 || f.FSType != "xfs" {
		t.Fatalf("%+v", res.Disk)
	}
	var seq []string
	for _, c := range x.fx.calls[1:] { // after the list
		if c[0] == "lsblk" {
			continue
		}
		seq = append(seq, strings.Join(c, " "))
	}
	want := []string{
		"chattr +i " + x.real,
		"nsenter -t 1 -m -- mount -t xfs -o prjquota UUID=u-new-1234 /var/lib/stackr-disks/d1",
		"nsenter -t 1 -m -- systemctl daemon-reload",
		`nsenter -t 1 -m -- systemctl enable var-lib-stackr\x2ddisks-d1.mount`,
	}
	if strings.Join(seq, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(seq, "\n"))
	}
	b, err := os.ReadFile(filepath.Join(x.env.Root, "etc/systemd/system", `var-lib-stackr\x2ddisks-d1.mount`))
	if err != nil || string(b) != unitText(MountPoint("d1"), "u-new-1234") {
		t.Fatalf("unit %q %v", b, err)
	}
	// a second run only checks: no chattr, no mount
	x.fx.calls = nil
	wantCode(t, x.mount(), "")
	if len(x.fx.ran("chattr"))+len(x.fx.ran("mount")) != 0 {
		t.Fatalf("%v", x.fx.calls)
	}
}

func TestMountRefusals(t *testing.T) { // DL
	t.Run("not the listed device", func(t *testing.T) {
		x := newX(t, storedDisk())
		d := x.listed()
		d.FSUUID = "u-other-1234"
		wantCode(t, x.run("mount", func(q *Request) { q.Device, q.Expect = d.ID, &d }), CodeDeviceChanged)
		wantCode(t, x.run("mount", func(q *Request) { q.Device = d.ID }), CodeDeviceChanged)
	})
	t.Run("a foreign filesystem", func(t *testing.T) {
		x := newX(t, dev("/dev/sdb", "disk", "fstype", "xfs", "uuid", "u-new-1234"))
		wantCode(t, x.mount(), CodeForeignStamp)
	})
	t.Run("an unformatted device", func(t *testing.T) {
		x := newX(t, plain())
		wantCode(t, x.mount(), CodeForeignStamp)
	})
	t.Run("mounted elsewhere", func(t *testing.T) {
		x := newX(t, dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-new-1234", "mountpoints", []any{"/mnt/other"}))
		wantCode(t, x.mount(), CodeDeviceBlocked)
	})
	t.Run("another device holds the mount point", func(t *testing.T) {
		x := newX(t, storedDisk())
		x.setTable("36 25 8:17 / /var/lib/stackr-disks/d1 rw - xfs /dev/sdc rw\n")
		wantCode(t, x.mount(), CodeBusy)
	})
	t.Run("a wrong mount point", func(t *testing.T) {
		x := newX(t, storedDisk())
		d := x.listed()
		wantCode(t, x.run("mount", func(q *Request) { q.Device, q.Expect, q.Mount = d.ID, &d, "/mnt/x" }), CodeBadName)
	})
	t.Run("a symlink on the way", func(t *testing.T) {
		x := newX(t, storedDisk())
		if err := os.RemoveAll(filepath.Join(x.env.Root, "var")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(x.env.Root, "elsewhere/lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("elsewhere", filepath.Join(x.env.Root, "var")); err != nil {
			t.Fatal(err)
		}
		wantCode(t, x.mount(), CodeBadName)
		if len(x.fx.ran("mount"))+len(x.fx.ran("chattr")) != 0 {
			t.Fatal("went on through a symlink")
		}
	})
	t.Run("a folder with files", func(t *testing.T) {
		x := newX(t, storedDisk())
		if err := os.WriteFile(filepath.Join(x.real, "f"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		wantCode(t, x.mount(), CodeNotEmpty)
		if len(x.fx.ran("mount")) != 0 {
			t.Fatal("mounted over files")
		}
	})
	t.Run("mount fails", func(t *testing.T) {
		x := newX(t, storedDisk())
		inner := x.fx.out
		x.fx.out = func(argv []string) ([]byte, error) {
			if argv[0] == "nsenter" && argv[5] == "mount" {
				return nil, errors.New("mount: wrong fs type")
			}
			return inner(argv)
		}
		wantCode(t, x.mount(), CodeIO)
	})
}

// mounted is a rig whose xfs disk is mounted and stamped.
func mountedX(t *testing.T) *xrig {
	x := newX(t, dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-new-1234", "size", 4<<30, "mountpoints", []any{"/var/lib/stackr-disks/d1"}))
	x.setTable(xfsLine)
	wantCode(t, x.run("add", nil), "")
	x.fx.calls = nil
	return x
}

func TestAddStampsXFSAndReportsTheDevice(t *testing.T) {
	x := newX(t, dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-new-1234", "size", 4<<30, "mountpoints", []any{"/var/lib/stackr-disks/d1"}))
	x.setTable(xfsLine)
	res := x.run("add", nil)
	wantCode(t, res, "")
	if f := res.Disk; f.Device != "/dev/sdb" || f.DeviceBytes != 4<<30 || f.FSUUID != "u-new-1234" {
		t.Fatalf("%+v", f)
	}
	b, _ := os.ReadFile(filepath.Join(x.real, stampFile))
	var s Stamp
	if json.Unmarshal(b, &s) != nil || s.Kind != "xfs" || s.ID != "d1" || s.Owner != "n1" {
		t.Fatalf("stamp %s", b)
	}
	if _, err := os.Stat(filepath.Join(x.real, "d1")); err != nil {
		t.Fatal("no id folder")
	}
}

func TestXFSOpsRefuseWhatIsNotTheXFSMountOfThisDisk(t *testing.T) { // DL
	x := newX(t, plain())
	for _, op := range []string{"add", "verify", "folder", "usage", "drop", "claim", "grow"} {
		wantCode(t, x.run(op, nil), CodeDiskMissing) // not mounted
	}
	x.setTable("36 25 8:17 / /var/lib/stackr-disks/d1 rw - ext4 /dev/sdb rw\n")
	for _, op := range []string{"add", "verify", "folder", "usage", "drop", "claim", "grow"} {
		wantCode(t, x.run(op, nil), CodeDiskMissing) // not xfs
	}
	x.setTable(xfsLine)
	for _, op := range []string{"add", "verify", "folder", "usage", "drop", "claim", "grow"} {
		wantCode(t, x.run(op, func(q *Request) { q.Mount = "/disk" }), CodeBadName)
	}
	wantCode(t, x.run("probe", nil), CodeUnsupported)
	if len(x.fx.ran("xfs_quota"))+len(x.fx.ran("xfs_growfs")) != 0 {
		t.Fatalf("%v", x.fx.calls)
	}
}

func TestVerifyAndClaimOnXFS(t *testing.T) {
	x := mountedX(t)
	res := x.run("verify", nil)
	wantCode(t, res, "")
	if res.Disk.FSUUID != "u-new-1234" {
		t.Fatalf("%+v", res.Disk)
	}
	wantCode(t, x.run("verify", func(q *Request) { q.Owner = "n9" }), CodeForeignStamp)
	wantCode(t, x.run("claim", func(q *Request) { q.Owner, q.From = "n2", "n1" }), "")
	wantCode(t, x.run("verify", func(q *Request) { q.Owner = "n2" }), "")
}

func TestGrowRunsXfsGrowfsOnTheMount(t *testing.T) {
	x := mountedX(t)
	res := x.run("grow", func(q *Request) { q.DeviceBytes, q.FSUUID = 2<<30, "u-new-1234" })
	wantCode(t, res, "")
	g := x.fx.ran("xfs_growfs")
	if len(g) != 1 || strings.Join(g[0], " ") != "xfs_growfs "+x.real {
		t.Fatalf("%v", g)
	}
	if res.Disk.DeviceBytes != 4<<30 {
		t.Fatalf("%+v", res.Disk)
	}
}

func TestGrowRefusedWhenTheDeviceIsSmaller(t *testing.T) { // DL
	x := mountedX(t)
	wantCode(t, x.run("grow", func(q *Request) { q.DeviceBytes, q.FSUUID = 8<<30, "u-new-1234" }), CodeDeviceSmaller)
	if len(x.fx.ran("xfs_growfs")) != 0 {
		t.Fatal("grew a shrunk device")
	}
}

func TestGrowFailureIsIO(t *testing.T) {
	x := mountedX(t)
	inner := x.fx.out
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "xfs_growfs" {
			return nil, errors.New("xfs_growfs: no space")
		}
		return inner(argv)
	}
	wantCode(t, x.run("grow", func(q *Request) { q.FSUUID = "u-new-1234" }), CodeIO)
}

const reportFixture = `#0                  0          0          0     00 [--------]
#41               4096          0          0     00 [--------]
#42            1048576          0    2097152     00 [--------]
junk line
`

func TestUsageFromQuotaReportParser(t *testing.T) {
	m := parseReport([]byte(reportFixture))
	if len(m) != 3 || m[42] != 1048576*1024 || m[41] != 4096*1024 || m[0] != 0 {
		t.Fatalf("%v", m)
	}
	if m := parseReport([]byte("Project quota on /x (/dev/sdb)\n  Blocks\nProject ID  Used  Soft  Hard\n")); len(m) != 0 {
		t.Fatalf("%v", m)
	}
}

// quotas answers xfs_quota: the report fixture with the hard limits set so
// far (a limit command is remembered, like the real quota), every command is
// recorded.
func quotas(x *xrig, report string) {
	inner := x.fx.out
	hard := map[string]string{}
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "xfs_quota" {
			var p, v string
			if n, _ := fmt.Sscanf(argv[3], "limit -p bhard=%s %s", &v, &p); n == 2 {
				hard[p] = strings.TrimSuffix(v, "k")
			}
			if argv[3] == "report -p -b -N" {
				out := report
				for p, h := range hard {
					out += "#" + p + "  0  0  " + h + "  00 [--------]\n"
				}
				return []byte(out), nil
			}
		}
		return inner(argv)
	}
}

func cmds(x *xrig) []string {
	var out []string
	for _, c := range x.fx.ran("xfs_quota") {
		out = append(out, c[3])
	}
	return out
}

func TestFolderSetsTheProjectAndTheCap(t *testing.T) {
	x := mountedX(t)
	quotas(x, reportFixture)
	res := x.run("folder", func(q *Request) {
		q.Names = []string{"vol"}
		q.Quotas = map[string]Quota{"vol": {Project: 41, Cap: 5 << 20}}
	})
	wantCode(t, res, "")
	if _, err := os.Stat(filepath.Join(x.real, "d1/vol")); err != nil {
		t.Fatal("no folder")
	}
	want := []string{"project -s -p " + x.real + "/d1/vol 41", "report -p -b -N", "limit -p bhard=5120k 41", "report -p -b -N"}
	if strings.Join(cmds(x), "|") != strings.Join(want, "|") {
		t.Fatalf("%v", cmds(x))
	}
	if c := x.fx.ran("xfs_quota")[0]; strings.Join(c[:3], " ") != "xfs_quota -x -c" || c[4] != x.real {
		t.Fatalf("%v", c)
	}
}

func TestFolderSkipsTheProjectOnlyWhenTheFolderAlreadyHasIt(t *testing.T) { // X6
	for _, c := range []struct {
		name    string
		proj    uint32
		inherit bool
		err     error
		skipped bool
	}{
		{"set and inherited", 41, true, nil, true},
		{"another project", 40, true, nil, false},
		{"no inherit flag", 41, false, nil, false},
		{"a fresh folder", 0, false, nil, false},
		{"unreadable", 0, false, errors.New("ENOTTY"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := mountedX(t)
			quotas(x, reportFixture)
			x.env.ProjOf = func(dir string) (uint32, bool, error) {
				if dir != x.real+"/d1/vol" {
					t.Fatalf("asked about %s", dir)
				}
				return c.proj, c.inherit, c.err
			}
			wantCode(t, x.run("folder", func(q *Request) {
				q.Names = []string{"vol"}
				q.Quotas = map[string]Quota{"vol": {Project: 41, Cap: 5 << 20}}
			}), "")
			got := cmds(x)
			if c.skipped != (got[0] == "report -p -b -N") {
				t.Fatalf("%v", got)
			}
		})
	}
}

func TestFolderFailsWhenTheLimitIsNotThereAfterwards(t *testing.T) { // X13
	x := mountedX(t)
	quotas(x, "")
	inner := x.fx.out
	x.fx.out = func(argv []string) ([]byte, error) { // prints an error, exits 0, sets nothing
		if argv[0] == "xfs_quota" && strings.HasPrefix(argv[3], "limit") {
			return nil, nil
		}
		return inner(argv)
	}
	wantCode(t, x.run("folder", func(q *Request) {
		q.Names, q.Quotas = []string{"vol"}, map[string]Quota{"vol": {Project: 7, Cap: 1 << 20}}
	}), CodeIO)
}

func TestReadDeviceSizeAndUUIDFromSysfs(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"dev/disk/by-id", "dev/disk/by-uuid", "sys/class/block/sdb"} {
		must(os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	must(os.Symlink("../../sdb", filepath.Join(root, "dev/disk/by-id/ata-X")))
	must(os.Symlink("../../sdb", filepath.Join(root, "dev/disk/by-uuid/u-1")))
	must(os.WriteFile(filepath.Join(root, "sys/class/block/sdb/size"), []byte("4096\n"), 0o644))
	size, uuid, err := ReadDevice(root, "/dev/disk/by-id/ata-X")
	if err != nil || size != 4096*512 || uuid != "u-1" {
		t.Fatalf("%d %q %v", size, uuid, err)
	}
	if _, _, err := ReadDevice(root, "/dev/disk/by-id/none"); err == nil {
		t.Fatal("a missing device read")
	}
}

func TestCapReappliedOnPlaceAndClearedWithZero(t *testing.T) {
	x := mountedX(t)
	quotas(x, "")
	for _, c := range []int64{1 << 20, 0} {
		x.fx.calls = nil
		wantCode(t, x.run("folder", func(q *Request) {
			q.Names, q.Quotas = []string{"vol"}, map[string]Quota{"vol": {Project: 7, Cap: c}}
		}), "")
		last := cmds(x)[2]
		if (c == 0) != (last == "limit -p bhard=0 7") || (c != 0 && last != "limit -p bhard=1024k 7") {
			t.Fatalf("%v", cmds(x))
		}
	}
}

func TestCapLowerThanUseRefused(t *testing.T) { // DL
	x := mountedX(t)
	quotas(x, reportFixture) // project 42 holds 1 GiB
	res := x.run("folder", func(q *Request) {
		q.Names, q.Quotas = []string{"vol"}, map[string]Quota{"vol": {Project: 42, Cap: 1 << 20}}
	})
	wantCode(t, res, CodeCapBelowUse)
	for _, c := range cmds(x) {
		if strings.HasPrefix(c, "limit") {
			t.Fatalf("limit set: %v", cmds(x))
		}
	}
}

func TestFolderNeedsAProjectNumber(t *testing.T) {
	x := mountedX(t)
	for _, q := range []map[string]Quota{nil, {"vol": {}}, {"vol": {Project: 3, Cap: -1}}} {
		wantCode(t, x.run("folder", func(r *Request) { r.Names, r.Quotas = []string{"vol"}, q }), CodeBadName)
	}
	if _, err := os.Stat(filepath.Join(x.real, "d1/vol")); err == nil || len(x.fx.ran("xfs_quota")) != 0 {
		t.Fatal("folder made without a project")
	}
}

func TestUsageComesFromTheReportNotAWalk(t *testing.T) {
	x := mountedX(t)
	quotas(x, reportFixture)
	for _, n := range []string{"full", "empty"} {
		if err := os.MkdirAll(filepath.Join(x.real, "d1", n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(x.real, "d1/full/f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := x.run("usage", func(q *Request) {
		q.Names, q.Sizes = []string{"full", "empty", "gone"}, true
		q.Quotas = map[string]Quota{"full": {Project: 42}, "empty": {Project: 41}, "gone": {Project: 5}}
	})
	wantCode(t, res, "")
	if u := res.Usage["full"]; u.Bytes != 1048576*1024 || u.Empty {
		t.Fatalf("%+v", u)
	}
	if !res.Usage["empty"].Empty || res.Usage["empty"].Bytes != 0 || !res.Usage["gone"].Missing {
		t.Fatalf("%+v", res.Usage)
	}
	// without sizes there is no report call at all
	x.fx.calls = nil
	wantCode(t, x.run("usage", func(q *Request) { q.Names = []string{"full"} }), "")
	if len(x.fx.ran("xfs_quota")) != 0 {
		t.Fatal("report without sizes")
	}
}

func TestXFSDropKeepsTheSiblings(t *testing.T) {
	x := mountedX(t)
	for _, n := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(x.real, "d1", n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wantCode(t, x.run("drop", func(q *Request) { q.Names = []string{"a"} }), "")
	if _, err := os.Stat(filepath.Join(x.real, "d1/b")); err != nil {
		t.Fatal("sibling gone")
	}
}

func TestWireRoundTripKeepsTheXFSFields(t *testing.T) {
	d := Device{Path: "/dev/sdb", ID: "/dev/disk/by-id/x", Bytes: 5, FSUUID: "u", Signature: "table"}
	in := Request{Op: "format", Kind: "xfs", Device: d.ID, DeviceBytes: 9, Expect: &d, Force: true, Quotas: map[string]Quota{"v": {Project: 2, Cap: 3}}}
	out, err := DecodeRequest(EncodeRequest(in))
	if err != nil || out.Expect == nil || !sameDevice(*out.Expect, d) || !out.Force || out.DeviceBytes != 9 || out.Quotas["v"] != (Quota{Project: 2, Cap: 3}) {
		t.Fatalf("%+v %v", out, err)
	}
	r, ok := ParseResult([]byte(EncodeResult(Result{Devices: []Device{d}, Disk: &Fact{Device: "x", DeviceBytes: 1, FSUUID: "u"}})))
	if !ok || !sameDevice(r.Devices[0], d) || r.Disk.FSUUID != "u" || r.Disk.DeviceBytes != 1 {
		t.Fatalf("%+v", r)
	}
	// the path kind's wire is unchanged
	b, _ := json.Marshal(Request{Op: "probe", Kind: "path", Mount: "/d"})
	if strings.Contains(string(b), "device") || strings.Contains(string(b), "force") || strings.Contains(string(b), "quotas") {
		t.Fatalf("%s", b)
	}
}

func TestFormatRechecksTheNodeBeforeEachDestructiveCommand(t *testing.T) { // DL, X1
	seq := func(x *xrig) string {
		var out []string
		for _, c := range x.fx.calls {
			if c[0] == "stat" || c[0] == "wipefs" || c[0] == "mkfs.xfs" {
				out = append(out, c[0])
			}
		}
		return strings.Join(out, " ")
	}
	x := newX(t, dev("/dev/sdb", "disk", "fstype", "ext4", "uuid", "u-1"))
	formatted(x)
	wantCode(t, x.format(true), "")
	if got := seq(x); got != "stat wipefs stat mkfs.xfs" {
		t.Fatalf("order %q", got)
	}
	// the name now belongs to another disk: nothing runs
	x = newX(t, dev("/dev/sdb", "disk", "fstype", "ext4", "uuid", "u-1"))
	d := x.listed()
	x.rdev = "8:32"
	wantCode(t, x.run("format", func(q *Request) { q.Device, q.Expect, q.Force = d.ID, &d, true }), CodeDeviceChanged)
	if x.fx.destructive() {
		t.Fatalf("ran on another disk: %v", x.fx.calls)
	}
	// it moves after wipefs: no mkfs
	x = newX(t, dev("/dev/sdb", "disk", "fstype", "ext4", "uuid", "u-1"))
	d = x.listed()
	inner := x.fx.out
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "wipefs" {
			x.rdev = "8:32"
		}
		return inner(argv)
	}
	wantCode(t, x.run("format", func(q *Request) { q.Device, q.Expect, q.Force = d.ID, &d, true }), CodeDeviceChanged)
	if len(x.fx.ran("wipefs")) != 1 || len(x.fx.ran("mkfs.xfs")) != 0 {
		t.Fatalf("%v", x.fx.calls)
	}
}

func TestFormatChecksTheIdentityAgainAfterWipefs(t *testing.T) { // DL, X1
	x := newX(t, dev("/dev/sdb", "disk", "fstype", "ext4", "uuid", "u-1"))
	d := x.listed()
	inner := x.fx.out
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "wipefs" { // another disk now has the name
			x.lsblk = lsJSON(sysDisk, dev("/dev/sdb", "disk", "serial", "other"))
		}
		return inner(argv)
	}
	wantCode(t, x.run("format", func(q *Request) { q.Device, q.Expect, q.Force = d.ID, &d, true }), CodeDeviceChanged)
	if len(x.fx.ran("mkfs.xfs")) != 0 {
		t.Fatalf("%v", x.fx.calls)
	}
}

func TestFormatRunsOnTheByIDPathWhenThereIsOne(t *testing.T) { // X1
	x := newX(t, plain())
	dir := filepath.Join(x.env.Root, "dev/disk/by-id")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../sdb", filepath.Join(dir, "scsi-0QEMU_a")); err != nil {
		t.Fatal(err)
	}
	formatted(x)
	wantCode(t, x.format(false), "")
	m := x.fx.ran("mkfs.xfs")
	if len(m) != 1 || m[0][len(m[0])-1] != filepath.Join(dir, "scsi-0QEMU_a") {
		t.Fatalf("%v", m)
	}
	for _, c := range x.fx.calls {
		if c[0] == "stat" && c[1] != filepath.Join(dir, "scsi-0QEMU_a") {
			t.Fatalf("stat of %v", c)
		}
	}
}

func TestGrowRefusedWhenAnotherFilesystemIsMounted(t *testing.T) { // DL, X3
	x := mountedX(t)
	for _, uuid := range []string{"", "u-other-1234"} {
		wantCode(t, x.run("grow", func(q *Request) { q.FSUUID = uuid }), CodeDeviceChanged)
	}
	if len(x.fx.ran("xfs_growfs")) != 0 {
		t.Fatal("grew another filesystem")
	}
}

func TestMountRetryWhenTheHelperReportsMountsUnderHost(t *testing.T) { // X7
	x := newX(t, dev("/dev/sdb", "disk", "fstype", "xfs", "label", "stackr", "uuid", "u-new-1234",
		"mountpoints", []any{"/host/var/lib/stackr-disks/d1"}))
	x.setTable(xfsLine)
	wantCode(t, x.mount(), "")
	// mounted somewhere else too: still refused
	x.setTable(xfsLine + "40 25 8:99 / /mnt/y rw - xfs /dev/sdb rw\n")
	wantCode(t, x.mount(), CodeDeviceBlocked)
}

func TestMountRefusesAForeignUnitAndKeepsOurs(t *testing.T) { // X10
	x := newX(t, storedDisk())
	mounting(x)
	unit := filepath.Join(x.env.Root, "etc/systemd/system", unitName(MountPoint("d1")))
	if err := os.WriteFile(unit, []byte("[Mount]\nWhat=/dev/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantCode(t, x.mount(), CodeBusy)
	if b, _ := os.ReadFile(unit); string(b) != "[Mount]\nWhat=/dev/other\n" {
		t.Fatalf("overwritten: %q", b)
	}
	if len(x.fx.ran("mount")) != 0 {
		t.Fatal("mounted with a foreign unit in place")
	}
	// a unit of ours for another uuid is also not ours for this disk
	if err := os.WriteFile(unit, []byte(unitText(MountPoint("d1"), "u-old-12345")), 0o644); err != nil {
		t.Fatal(err)
	}
	wantCode(t, x.mount(), CodeBusy)
	// exactly ours: accepted
	if err := os.WriteFile(unit, []byte(unitText(MountPoint("d1"), "u-new-1234")), 0o644); err != nil {
		t.Fatal(err)
	}
	wantCode(t, x.mount(), "")
}

func TestUnmountTakesTheDiskOffTheHost(t *testing.T) { // X9
	x := mountedX(t)
	unit := filepath.Join(x.env.Root, "etc/systemd/system", unitName(MountPoint("d1")))
	if err := os.WriteFile(unit, []byte(unitText(MountPoint("d1"), "u-new-1234")), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := x.fx.out
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "nsenter" && argv[5] == "umount" {
			x.setTable("")
		}
		return inner(argv)
	}
	// the stamp and the id folder are in the way of rmdir, as on a real disk they
	// live on the unmounted filesystem; here the folder is the temp stand-in
	if err := os.RemoveAll(x.real); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(x.real, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCode(t, x.run("unmount", nil), "")
	var seq []string
	for _, c := range x.fx.calls {
		seq = append(seq, strings.Join(c, " "))
	}
	want := []string{
		"nsenter -t 1 -m -- umount /var/lib/stackr-disks/d1",
		`nsenter -t 1 -m -- systemctl disable var-lib-stackr\x2ddisks-d1.mount`,
		"nsenter -t 1 -m -- systemctl daemon-reload",
		"chattr -i " + x.real,
	}
	if strings.Join(seq, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(seq, "\n"))
	}
	if _, err := os.Stat(unit); err == nil {
		t.Fatal("unit kept")
	}
	if _, err := os.Stat(x.real); err == nil {
		t.Fatal("mount point kept")
	}
	// done twice is fine
	wantCode(t, x.run("unmount", nil), "")
}

func TestUnmountRefusals(t *testing.T) { // DL, X9
	x := mountedX(t)
	x.setTable(xfsLine + "40 36 8:99 / /var/lib/stackr-disks/d1/vol rw - ext4 /dev/sdc rw\n")
	wantCode(t, x.run("unmount", nil), CodeBusy)
	if len(x.fx.ran("umount")) != 0 {
		t.Fatal("unmounted with a mount inside")
	}
	x.setTable("36 25 8:17 / /var/lib/stackr-disks/d1 rw - ext4 /dev/sdb rw\n")
	wantCode(t, x.run("unmount", nil), CodeDiskMissing)
	wantCode(t, x.run("unmount", func(q *Request) { q.Mount = "/mnt/x" }), CodeBadName)
	x.setTable(xfsLine)
	inner := x.fx.out
	x.fx.out = func(argv []string) ([]byte, error) {
		if argv[0] == "nsenter" && argv[5] == "umount" {
			return nil, errors.New("target is busy")
		}
		return inner(argv)
	}
	x.fx.calls = nil
	wantCode(t, x.run("unmount", nil), CodeBusy)
	if len(x.fx.ran("systemctl"))+len(x.fx.ran("chattr")) != 0 {
		t.Fatalf("went on after a failed umount: %v", x.fx.calls)
	}
}

func TestUnmountOfADiskThatIsNotMountedAndHasNoUnit(t *testing.T) { // X9: a retry, a half-added disk
	x := newX(t, plain())
	if err := os.WriteFile(filepath.Join(x.real, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wantCode(t, x.run("unmount", nil), "")
	for _, p := range []string{"umount", "systemctl"} {
		if len(x.fx.ran(p)) != 0 {
			t.Fatalf("%s ran: %v", p, x.fx.calls)
		}
	}
	if _, err := os.Stat(filepath.Join(x.real, "keep")); err != nil {
		t.Fatal("a file in the mount point was removed")
	}
}
