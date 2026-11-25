// Package hostdisk is the node-disk code that runs on a node's own
// filesystem: in the host helper (`stackrd host-run disk`) with the host's /
// at /host, or in process. It imports only the standard library.
//
// A disk is a mounted path carrying a stamp file, <mount>/.stackr-disk. A
// volume lives in <mount>/<disk id>/<docker volume name>.
package hostdisk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Env is the machine the code runs against. Tests set Root to t.TempDir()
// and Mountinfo to a file they wrote; nothing outside Root is touched.
type Env struct {
	Root      string // "/host" in the helper, "/" in process, a temp dir in tests
	Mountinfo string // default Root+"/proc/1/mountinfo"
	// TempMounts is for tests: a folder in a temp dir stands in for a mount
	// and shares its parent's device, so the same-device refusal is skipped.
	TempMounts bool
	// Exec runs a command (mkfs.xfs, wipefs, xfs_quota, xfs_growfs, lsblk,
	// nsenter, chattr) and returns its stdout. Nil runs the real thing;
	// tests set a recording fake, so no device is ever touched.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)

	beforeTakeover func() // test seam: runs between the emptiness check and the takeover
	beforeStamp    func() // test seam: runs between the emptiness check and the first stamp write
	// StatRdev and ProjOf are test seams: the "major:minor" of a device node, and a folder's xfs project and inherit flag. Nil reads the real node and runs the real ioctl.
	StatRdev func(path string) (string, error)                          // test seam: "major:minor" of a device node
	ProjOf   func(dir string) (project uint32, inherit bool, err error) // test seam: a folder's xfs project and inherit flag
}

func (e Env) mountinfo() string {
	if e.Mountinfo != "" {
		return e.Mountinfo
	}
	return strings.TrimSuffix(e.Root, "/") + "/proc/1/mountinfo"
}

// Request ops. path: probe, add, verify, folder, usage, drop, claim.
// xfs: list, format, mount, unmount, add, verify, folder, usage, drop, claim, grow.
type Request struct {
	Op      string   `json:"op"`
	Kind    string   `json:"kind"` // "path" or "xfs"
	DiskID  string   `json:"disk_id,omitempty"`
	Owner   string   `json:"owner,omitempty"`   // the node id the stamp must carry
	From    string   `json:"from,omitempty"`    // claim: the node that owned the stamp before
	Install string   `json:"install,omitempty"` // add: written into the stamp
	Mount   string   `json:"mount,omitempty"`
	Names   []string `json:"names,omitempty"` // folder, usage, drop: single path elements
	Sizes   bool     `json:"sizes,omitempty"` // usage: false = only "is it empty"

	// xfs only.
	Device      string           `json:"device,omitempty"`       // format, mount: a Device.ID or Device.Path from list
	FSUUID      string           `json:"fs_uuid,omitempty"`      // grow, unmount: the filesystem uuid recorded at add; another filesystem behind the mount is refused
	DeviceID    string           `json:"device_id,omitempty"`    // grow, unmount: the Device.ID recorded at add; grow requires it
	DeviceBytes int64            `json:"device_bytes,omitempty"` // grow: the size recorded at add; a smaller device is refused
	Expect      *Device          `json:"expect,omitempty"`       // format, mount: the device as list showed it; any difference now aborts
	Force       bool             `json:"force,omitempty"`        // format: overwrite a stackr stamp or a foreign filesystem signature (never a blocked device)
	Quotas      map[string]Quota `json:"quotas,omitempty"`       // folder, usage: by folder name
}

// Quota is a folder's xfs project: its number (unique per disk) and the hard
// cap in bytes, 0 for none. folder sets both every time it runs.
type Quota struct {
	Project uint32 `json:"project"`
	Cap     int64  `json:"cap,omitempty"`
}

type Use struct {
	Bytes   int64 `json:"bytes"`
	Empty   bool  `json:"empty"`
	Missing bool  `json:"missing"`
}

type Fact struct {
	Path         string    `json:"path"`
	StampID      string    `json:"stamp_id"`
	StampOwner   string    `json:"stamp_owner"`
	StampInstall string    `json:"stamp_install"`
	FSType       string    `json:"fs_type"`
	Mounted      bool      `json:"mounted"`
	RootFS       bool      `json:"root_fs"`
	Network      bool      `json:"network"`
	Empty        bool      `json:"empty"`
	Free         uint64    `json:"free"`
	Total        uint64    `json:"total"`
	Device       string    `json:"device,omitempty"`       // xfs: Device.ID of the block device behind the mount
	DeviceBytes  int64     `json:"device_bytes,omitempty"` // xfs: its size now
	FSUUID       string    `json:"fs_uuid,omitempty"`      // xfs: the filesystem uuid
	ReadAt       time.Time `json:"read_at"`                // set by the worker: when this read happened
}

// Error codes are typed; callers never match on the message.
const (
	CodeDiskMissing   = "disk_missing"
	CodeNotMountPoint = "not_mount_point"
	CodeRootFS        = "root_fs"
	CodeNotEmpty      = "not_empty"
	CodeForeignStamp  = "foreign_stamp"
	CodeBadName       = "bad_name"
	CodeUnsupported   = "unsupported"
	CodeReadOnly      = "read_only" // probe: the disk is mounted read-only
	CodeIO            = "io"        // a filesystem call failed for a reason that is not a refusal

	// xfs
	CodeContainer     = "container"      // the node is itself a container and cannot mount a device
	CodeNoDevice      = "no_device"      // the device is not on this machine
	CodeDeviceChanged = "device_changed" // not the device list showed
	CodeDeviceBlocked = "device_blocked" // a format guard with no override; Result.Devices[0].Blocked says which
	CodeNeedsForce    = "needs_force"    // a stamp or filesystem signature is there and Force is off
	CodeDeviceSmaller = "device_smaller" // grow: the device is smaller than recorded
	CodeCapBelowUse   = "cap_below_use"  // folder: the cap is under what the folder already holds
	CodeBusy          = "busy"           // mount: the mount point or the device is in use by something else
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type Result struct {
	Error *Error         `json:"error,omitempty"`
	Disk  *Fact          `json:"disk,omitempty"`
	Usage map[string]Use `json:"usage,omitempty"`
	// list: every disk, with why it is not pickable. format: the device after.
	Devices []Device `json:"devices,omitempty"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func badName(n string) *Error {
	if nameRe.MatchString(n) {
		return nil
	}
	return &Error{CodeBadName, "not a single path element: " + n}
}

// Run does one request.
func Run(ctx context.Context, e Env, r Request) Result {
	if err := ctx.Err(); err != nil {
		return fail(CodeIO, err.Error())
	}
	k, ok := kinds[r.Kind]
	if !ok {
		return fail(CodeUnsupported, "disk kind "+r.Kind)
	}
	return k.run(ctx, e, r)
}

func fail(code, msg string) Result { return Result{Error: &Error{code, msg}} }

// ResultPrefix starts the one stdout line that carries a Result. The helper's
// logs merge stdout and stderr, so the reader takes the last line with it.
const ResultPrefix = "stackr-disk-result: "

// EncodeResult is the result line, without the newline.
func EncodeResult(r Result) string {
	b, _ := json.Marshal(r)
	return ResultPrefix + string(b)
}

// ParseResult takes the last result line out of the helper's output. ok is
// false when there is none (a crash).
func ParseResult(out []byte) (r Result, ok bool) {
	lines := bytes.Split(out, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(string(lines[i]))
		if !strings.HasPrefix(l, ResultPrefix) {
			continue
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(l, ResultPrefix)), &r) != nil {
			return Result{}, false
		}
		return r, true
	}
	return Result{}, false
}

// EncodeRequest is the helper's command line argument.
func EncodeRequest(r Request) string {
	b, _ := json.Marshal(r)
	return base64.StdEncoding.EncodeToString(b)
}

func DecodeRequest(s string) (Request, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Request{}, err
	}
	var r Request
	if err := json.Unmarshal(b, &r); err != nil {
		return Request{}, errors.New("bad request: " + err.Error())
	}
	return r, nil
}

// Stamp is <mount>/.stackr-disk.
type Stamp struct {
	ID      string    `json:"id"`
	Install string    `json:"install"`
	Kind    string    `json:"kind"`
	Owner   string    `json:"owner"`
	Created time.Time `json:"created"`
}
