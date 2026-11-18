package hostdisk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Device is one whole block device as lsblk shows it, with what stackr
// concludes about it. The picker shows these; format takes one back as
// Request.Expect and aborts when the device now differs.
type Device struct {
	Path   string   `json:"path"`             // kernel node, /dev/sdb
	ID     string   `json:"id"`               // stable name: the /dev/disk/by-id link, else Path
	Model  string   `json:"model,omitempty"`  //
	Serial string   `json:"serial,omitempty"` //
	WWN    string   `json:"wwn,omitempty"`    //
	Bytes  int64    `json:"bytes"`
	FSType string   `json:"fs_type,omitempty"`
	FSUUID string   `json:"fs_uuid,omitempty"`
	Label  string   `json:"label,omitempty"`
	PTType string   `json:"pt_type,omitempty"` // partition table type
	PTUUID string   `json:"pt_uuid,omitempty"`
	Mounts []string `json:"mounts,omitempty"`
	// Blocked is why the device can never be formatted, no override:
	// system, mounted, partitions, swap, lvm, md, luks, zfs, holders, readonly.
	Blocked string `json:"blocked,omitempty"`
	// Signature is what format would destroy and only Force may: "stackr"
	// (an earlier stackr disk), "filesystem" (a foreign one), "table".
	Signature string `json:"signature,omitempty"`
}

// sameDevice is the identity compare between the list and the format: the
// derived fields (mounts, blocked, signature) are not part of it.
func sameDevice(a, b Device) bool {
	return a.Path == b.Path && a.ID == b.ID && a.Model == b.Model && a.Serial == b.Serial && a.WWN == b.WWN &&
		a.Bytes == b.Bytes && a.FSType == b.FSType && a.FSUUID == b.FSUUID && a.Label == b.Label &&
		a.PTType == b.PTType && a.PTUUID == b.PTUUID
}

// stackrLabel is the filesystem label mkfs writes, so a stackr disk can be
// told from a foreign filesystem without mounting it.
const stackrLabel = "stackr"

// exec runs a command through Env.Exec, or really.
func (e Env) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if e.Exec != nil {
		return e.Exec(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return out.Bytes(), fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return out.Bytes(), nil
}

const lsblkCols = "NAME,KNAME,PATH,TYPE,SIZE,RO,FSTYPE,UUID,LABEL,PTTYPE,PTUUID,MODEL,SERIAL,WWN,MOUNTPOINTS"

// lsNode is one lsblk JSON entry. Older util-linux writes numbers and
// booleans as strings, newer as numbers and booleans.
type lsNode struct {
	Kname       string      `json:"kname"`
	Path        string      `json:"path"`
	Type        string      `json:"type"`
	Size        json.Number `json:"size"`
	RO          flexBool    `json:"ro"`
	FSType      *string     `json:"fstype"`
	UUID        *string     `json:"uuid"`
	Label       *string     `json:"label"`
	PTType      *string     `json:"pttype"`
	PTUUID      *string     `json:"ptuuid"`
	Model       *string     `json:"model"`
	Serial      *string     `json:"serial"`
	WWN         *string     `json:"wwn"`
	Mountpoints []*string   `json:"mountpoints"`
	Children    []lsNode    `json:"children"`
}

type flexBool bool

func (b *flexBool) UnmarshalJSON(p []byte) error {
	s := strings.Trim(string(p), `"`)
	*b = s == "true" || s == "1"
	return nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// all calls fn for n and everything under it.
func (n lsNode) all(fn func(lsNode)) {
	fn(n)
	for _, c := range n.Children {
		c.all(fn)
	}
}

func (n lsNode) mounts() []string {
	var out []string
	for _, m := range n.Mountpoints {
		if s := str(m); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (e Env) lsblk(ctx context.Context) ([]lsNode, error) {
	out, err := e.exec(ctx, "lsblk", "-J", "-b", "-o", lsblkCols)
	if err != nil {
		return nil, err
	}
	var v struct {
		Blockdevices []lsNode `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("lsblk output: %w", err)
	}
	return v.Blockdevices, nil
}

// byIDLinks maps a kernel name (sdb) to its stable /dev/disk/by-id name.
// Partition links are skipped; a wwn- link is the last choice.
func byIDLinks(root string) map[string]string {
	const dir = "/dev/disk/by-id"
	ents, err := os.ReadDir(joinRoot(root, dir))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, d := range ents {
		if strings.Contains(d.Name(), "-part") {
			continue
		}
		dst, err := os.Readlink(filepath.Join(joinRoot(root, dir), d.Name()))
		if err != nil {
			continue
		}
		k, id := filepath.Base(dst), dir+"/"+d.Name()
		rank := func(s string) bool { return strings.HasPrefix(s, dir+"/wwn-") } // wwn links are the last choice
		if cur, ok := out[k]; !ok || (rank(id) == rank(cur) && id < cur) || (rank(cur) && !rank(id)) {
			out[k] = id
		}
	}
	return out
}

func (n lsNode) device(byID map[string]string) Device {
	size, _ := n.Size.Int64()
	d := Device{Path: n.Path, ID: n.Path, Model: str(n.Model), Serial: str(n.Serial), WWN: str(n.WWN), Bytes: size,
		FSType: str(n.FSType), FSUUID: str(n.UUID), Label: str(n.Label), PTType: str(n.PTType), PTUUID: str(n.PTUUID)}
	if id := byID[n.Kname]; id != "" {
		d.ID = id
	}
	n.all(func(c lsNode) { d.Mounts = append(d.Mounts, c.mounts()...) })
	return d
}

// assess sets Blocked and Signature. root is Env.Root, for the holders
// directory in sysfs; sys is the set of device nodes that carry / or the
// Docker root.
func assess(root string, n lsNode, d *Device, sys []string, mountSources []string) {
	var system, mounted, swap, parts bool
	n.all(func(c lsNode) {
		if slices.Contains(sys, c.Path) {
			system = true
		}
		for _, m := range c.mounts() {
			switch {
			case m == "/":
				system = true
				mounted = true
			case strings.HasPrefix(m, "[SWAP"):
				swap = true
			default:
				mounted = true
			}
		}
		if slices.Contains(mountSources, c.Path) {
			mounted = true
		}
		if c.Type == "part" {
			parts = true
		}
	})
	holders := len(n.Children) > 0
	if ents, err := os.ReadDir(joinRoot(root, "/sys/block/"+filepath.Base(n.Kname)+"/holders")); err == nil && len(ents) > 0 {
		holders = true
	}
	switch {
	case system:
		d.Blocked = "system"
	case mounted:
		d.Blocked = "mounted"
	case parts:
		d.Blocked = "partitions"
	case swap, d.FSType == "swap":
		d.Blocked = "swap"
	case d.FSType == "LVM2_member":
		d.Blocked = "lvm"
	case d.FSType == "linux_raid_member":
		d.Blocked = "md"
	case d.FSType == "crypto_LUKS":
		d.Blocked = "luks"
	case d.FSType == "zfs_member":
		d.Blocked = "zfs"
	case holders:
		d.Blocked = "holders"
	case bool(n.RO):
		d.Blocked = "readonly"
	}
	switch {
	case d.Blocked != "":
	case d.FSType == "xfs" && d.Label == stackrLabel:
		d.Signature = "stackr"
	case d.FSType != "":
		d.Signature = "filesystem"
	case d.PTType != "":
		d.Signature = "table"
	}
}

// devices lists the whole disks (not partitions, loops, optical drives or
// device-mapper nodes). nodes is the lsblk tree behind it.
func (e Env) devices(ctx context.Context) ([]Device, []lsNode, *Error) {
	nodes, err := e.lsblk(ctx)
	if err != nil {
		return nil, nil, &Error{CodeIO, "list the devices: " + err.Error()}
	}
	var sys, sources []string
	if table, err := os.ReadFile(e.mountinfo()); err == nil {
		ms := ParseMounts(table)
		for _, p := range []string{"/", "/var/lib/docker"} {
			if m, ok := lastCovering(ms, p); ok {
				sys = append(sys, m.Source)
			}
		}
		for _, m := range ms {
			sources = append(sources, m.Source)
		}
	} else {
		return nil, nil, &Error{CodeIO, "read the mount table: " + err.Error()}
	}
	byID := byIDLinks(e.Root)
	var out []Device
	var tops []lsNode
	for _, n := range nodes {
		if n.Type != "disk" {
			continue
		}
		d := n.device(byID)
		assess(e.Root, n, &d, sys, sources)
		out = append(out, d)
		tops = append(tops, n)
	}
	return out, tops, nil
}

// find is the listed device a request names, by ID or by Path.
func find(devs []Device, name string) (Device, bool) {
	if name == "" {
		return Device{}, false
	}
	for _, d := range devs {
		if d.ID == name || d.Path == name {
			return d, true
		}
	}
	return Device{}, false
}

func list(ctx context.Context, e Env) Result {
	devs, _, herr := e.devices(ctx)
	if herr != nil {
		return Result{Error: herr}
	}
	return Result{Devices: devs}
}
