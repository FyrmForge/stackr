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

	beforeTakeover func() // test seam: runs between the emptiness check and the takeover
	beforeStamp    func() // test seam: runs between the emptiness check and the first stamp write
}

func (e Env) mountinfo() string {
	if e.Mountinfo != "" {
		return e.Mountinfo
	}
	return strings.TrimSuffix(e.Root, "/") + "/proc/1/mountinfo"
}

// Request ops: probe, add, verify, folder, usage, drop.
type Request struct {
	Op      string   `json:"op"`
	Kind    string   `json:"kind"` // "path"
	DiskID  string   `json:"disk_id,omitempty"`
	Owner   string   `json:"owner,omitempty"`   // the node id the stamp must carry
	Install string   `json:"install,omitempty"` // add: written into the stamp
	Mount   string   `json:"mount,omitempty"`
	Names   []string `json:"names,omitempty"` // folder, usage, drop: single path elements
	Sizes   bool     `json:"sizes,omitempty"` // usage: false = only "is it empty"
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
	ReadAt       time.Time `json:"read_at"` // set by the worker: when this read happened
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
	if r.Kind != "path" {
		return fail(CodeUnsupported, "disk kind "+r.Kind)
	}
	switch r.Op {
	case "probe":
		return probe(e, r)
	case "add":
		return add(e, r)
	case "verify":
		return verify(e, r)
	case "folder":
		return folder(e, r)
	case "usage":
		return usage(ctx, e, r)
	case "drop":
		return drop(e, r)
	}
	return fail(CodeUnsupported, "op "+r.Op)
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
