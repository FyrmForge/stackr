package hostdisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	stampFile = ".stackr-disk"
	stampTmp  = ".stackr-disk.tmp" // the stamp is written here, then renamed
)

const brokenStamp = "A broken .stackr-disk file is here. Remove it, then retry."

var errBroken = errors.New("broken stamp")

// notThere: the entry is gone, or a parent is not a directory. Only this
// says the disk is missing; any other failure is an I/O error that must
// never latch a disk missing.
func notThere(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func ioOr(err error, missing string) *Error {
	if notThere(err) {
		return &Error{missing, err.Error()}
	}
	return &Error{CodeIO, err.Error()}
}

// stampErr turns a readStamp failure into a refusal; broken is the code for
// a stamp file that is empty or unparsable.
func stampErr(err error, broken string) *Error {
	if errors.Is(err, errBroken) {
		return &Error{broken, brokenStamp}
	}
	return &Error{CodeIO, "read the stamp: " + err.Error()}
}

// devOf is the device of a file as "major:minor", the way mountinfo says it.
func devOf(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	d := uint64(st.Dev) //nolint:unconvert // width differs by arch
	return fmt.Sprintf("%d:%d", (d>>8)&0xfff|(d>>32)&^0xfff, d&0xff|(d>>12)&^0xff)
}

// DevString is the device of p as "major:minor". Tests use it to write a
// mount table that matches a real directory.
func DevString(p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	return devOf(fi), nil
}

// realDir requires name under root to be a real directory: not a symlink,
// not a file.
func realDir(root *os.Root, name, missing string) *Error {
	fi, err := root.Lstat(name)
	if err != nil {
		return ioOr(err, missing)
	}
	if !fi.IsDir() {
		return &Error{CodeIO, name + " is not a folder"}
	}
	return nil
}

// target is a verified mount point, opened once. Every read and write of the
// disk goes through root, so a symlink or an unmount after the open can never
// reach the root filesystem.
type target struct {
	root   *os.Root
	mount  Mount
	mounts []Mount // the whole table
	dev    string  // device of the opened root
	path   string  // canonical, as the host sees it
	disk   string  // the path as the caller gave it
}

func (t *target) close() { _ = t.root.Close() }

// open resolves path, requires a mount point that is not the root
// filesystem, and opens it. missing is the code for "not a mount point":
// probe reports not_mount_point, every later op disk_missing.
func (e Env) open(path, missing string) (*target, *Error) {
	table, err := os.ReadFile(e.mountinfo())
	if err != nil {
		return nil, &Error{CodeIO, "read the mount table: " + err.Error()}
	}
	ms := ParseMounts(table)
	canon, err := resolveUnder(e.Root, path)
	if err != nil {
		herr := &Error{missing, err.Error()} // not absolute, too many links
		var pe *fs.PathError
		if errors.As(err, &pe) {
			herr = ioOr(err, missing)
		}
		herr.Message = path + ": " + herr.Message
		return nil, herr
	}
	m, ok := lastCovering(ms, canon)
	if !ok || m.Point != canon {
		return nil, &Error{missing, path + " is not a mount point"}
	}
	if rm, ok := lastCovering(ms, "/"); ok && rm.Dev == m.Dev {
		return nil, &Error{CodeRootFS, path + " is on the root filesystem"}
	}
	root, err := os.OpenRoot(joinRoot(e.Root, canon))
	if err != nil {
		return nil, ioOr(err, missing)
	}
	// an unmount between the table read and the open leaves the directory
	// of the parent filesystem here: the opened root must be the mounted device
	fi, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, ioOr(err, missing)
	}
	if devOf(fi) != m.Dev {
		_ = root.Close()
		return nil, &Error{missing, path + " is no longer the mounted filesystem"}
	}
	return &target{root: root, mount: m, mounts: ms, dev: m.Dev, path: canon, disk: path}, nil
}

func joinRoot(root, p string) string {
	if root == "" || root == "/" {
		return p
	}
	return root + p
}

// readStamp: found is false when there is no stamp file.
func readStamp(root *os.Root) (s Stamp, found bool, err error) {
	f, err := root.Open(stampFile)
	if errors.Is(err, fs.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return s, true, err
	}
	if err := json.Unmarshal(b, &s); err != nil || s.ID == "" {
		return Stamp{}, true, errBroken
	}
	return s, true, nil
}

// ReadStamp reads the stamp of the disk mounted at path, through the same
// mount checks as every op. A missing stamp is an error wrapping
// fs.ErrNotExist.
func ReadStamp(ctx context.Context, e Env, path string) (Stamp, error) {
	if err := ctx.Err(); err != nil {
		return Stamp{}, err
	}
	t, herr := e.open(path, CodeDiskMissing)
	if herr != nil {
		return Stamp{}, herr
	}
	defer t.close()
	s, found, err := readStamp(t.root)
	if err == nil && !found {
		err = fs.ErrNotExist
	}
	return s, err
}

// emptyDir: no entries, lost+found aside (Proxmox formats ext4).
func emptyDir(fsys fs.FS, dir string) (bool, error) {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return false, err
	}
	for _, d := range ents {
		if d.Name() != "lost+found" && d.Name() != stampTmp {
			return false, nil
		}
	}
	return true, nil
}

func (t *target) fact(s Stamp, empty bool) *Fact {
	f := &Fact{Path: t.disk, StampID: s.ID, StampOwner: s.Owner, StampInstall: s.Install,
		FSType: t.mount.FSType, Mounted: true, Network: IsNetwork(t.mount.FSType), Empty: empty}
	var st syscall.Statfs_t
	if syscall.Statfs(t.root.Name(), &st) == nil {
		f.Free, f.Total = uint64(st.Bavail)*uint64(st.Bsize), uint64(st.Blocks)*uint64(st.Bsize)
	}
	return f
}

func probe(e Env, r Request) Result {
	t, herr := e.open(r.Mount, CodeNotMountPoint)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	if herr := t.canonical(); herr != nil {
		return Result{Error: herr}
	}
	s, found, err := readStamp(t.root)
	if err != nil {
		return Result{Error: stampErr(err, CodeNotEmpty)}
	}
	empty, err := emptyDir(t.root.FS(), ".")
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	if !found && !empty {
		return fail(CodeNotEmpty, r.Mount+" is not empty and has no stamp")
	}
	return Result{Disk: t.fact(s, empty)}
}

func add(e Env, r Request) Result {
	if herr := badName(r.DiskID); herr != nil {
		return Result{Error: herr}
	}
	if r.Owner == "" {
		return fail(CodeBadName, "no owner")
	}
	t, herr := e.open(r.Mount, CodeDiskMissing)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	if herr := t.canonical(); herr != nil {
		return Result{Error: herr}
	}
	s, found, err := readStamp(t.root)
	switch {
	case err != nil:
		return Result{Error: stampErr(err, CodeNotEmpty)}
	case found && (s.ID != r.DiskID || s.Owner != r.Owner):
		return fail(CodeForeignStamp, "the disk carries another stamp")
	case !found:
		if empty, err := emptyDir(t.root.FS(), "."); err != nil {
			return fail(CodeIO, err.Error())
		} else if !empty {
			return fail(CodeNotEmpty, r.Mount+" is not empty and has no stamp")
		}
		s = Stamp{ID: r.DiskID, Install: r.Install, Kind: "path", Owner: r.Owner, Created: time.Now().UTC()}
		if err := writeStamp(t.root, s); err != nil {
			return fail(CodeIO, "write the stamp: "+err.Error())
		}
	}
	// a retry after a crash finds the stamp already there; the folder is made
	// once either way
	if err := t.root.Mkdir(r.DiskID, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fail(CodeIO, err.Error())
	}
	if herr := realDir(t.root, r.DiskID, CodeIO); herr != nil {
		return Result{Error: herr}
	}
	return Result{Disk: t.fact(s, false)}
}

// canonical refuses a path that is not already resolved, so the stored path,
// the worker's mount table compare and Docker all spell it one way.
func (t *target) canonical() *Error {
	if t.path != filepath.Clean(t.disk) {
		return &Error{CodeNotMountPoint, "Use the real path: " + t.path + "."}
	}
	return nil
}

// writeStamp writes a temp file, syncs it and renames it into place, so a
// crash never leaves an empty stamp.
func writeStamp(root *os.Root, s Stamp) error {
	b, _ := json.Marshal(s)
	_ = root.Remove(stampTmp) // a leftover of a crash
	f, err := root.OpenFile(stampTmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(stampTmp, stampFile)
	}
	if err != nil {
		_ = root.Remove(stampTmp)
	}
	return err
}

// verified opens the disk and checks the stamp and the disk id folder: the
// gate in front of folder, usage and drop. The caller closes the target.
func (e Env) verified(r Request) (*target, Stamp, *Error) {
	if herr := badName(r.DiskID); herr != nil {
		return nil, Stamp{}, herr
	}
	t, herr := e.open(r.Mount, CodeDiskMissing)
	if herr != nil {
		return nil, Stamp{}, herr
	}
	s, found, err := readStamp(t.root)
	switch {
	case err != nil:
		t.close()
		return nil, s, stampErr(err, CodeIO)
	case !found || s.ID != r.DiskID:
		t.close()
		return nil, s, &Error{CodeDiskMissing, "the disk is not the one registered"}
	case s.Owner != r.Owner:
		t.close()
		return nil, s, &Error{CodeForeignStamp, "the disk belongs to another node"}
	}
	if herr := realDir(t.root, r.DiskID, CodeDiskMissing); herr != nil {
		t.close()
		return nil, s, herr
	}
	return t, s, nil
}

func verify(e Env, r Request) Result {
	t, s, herr := e.verified(r)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	return Result{Disk: t.fact(s, false)}
}

func checkNames(names []string) *Error {
	for _, n := range names {
		if herr := badName(n); herr != nil {
			return herr
		}
	}
	return nil
}

func folder(e Env, r Request) Result {
	if herr := checkNames(r.Names); herr != nil {
		return Result{Error: herr}
	}
	t, s, herr := e.verified(r)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	for _, n := range r.Names {
		// one level only: a missing <id> folder is an error, never created here
		dir := r.DiskID + "/" + n
		if err := t.root.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			if errors.Is(err, fs.ErrNotExist) {
				return fail(CodeDiskMissing, "the disk has no folder for this id")
			}
			return fail(CodeIO, err.Error())
		}
		// an existing entry must be a real folder, not a file or a symlink
		if herr := realDir(t.root, dir, CodeIO); herr != nil {
			return Result{Error: herr}
		}
	}
	return Result{Disk: t.fact(s, false)}
}

func usage(ctx context.Context, e Env, r Request) Result {
	if herr := checkNames(r.Names); herr != nil {
		return Result{Error: herr}
	}
	t, _, herr := e.verified(r)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	fsys := t.root.FS()
	out := map[string]Use{}
	for _, n := range slices.Compact(slices.Sorted(slices.Values(r.Names))) {
		dir := r.DiskID + "/" + n
		if fi, err := fs.Stat(fsys, dir); err != nil || !fi.IsDir() {
			out[n] = Use{Missing: true, Empty: true}
			continue
		}
		u := Use{}
		ents, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return fail(CodeIO, err.Error())
		}
		u.Empty = len(ents) == 0
		if r.Sizes && !u.Empty {
			var err error
			u.Bytes, err = walkSize(ctx, fsys, dir)
			if err != nil {
				return fail(CodeIO, err.Error())
			}
		}
		out[n] = u
	}
	return Result{Usage: out}
}

func drop(e Env, r Request) Result {
	if herr := checkNames(r.Names); herr != nil {
		return Result{Error: herr}
	}
	t, _, herr := e.verified(r)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	for _, n := range r.Names {
		if herr := t.dropOne(r.DiskID + "/" + n); herr != nil {
			return Result{Error: herr}
		}
	}
	return Result{}
}

// dropOne removes dir and never crosses a filesystem boundary: it refuses
// when a mount point is dir or below it, or when anything under dir is on
// another device. Nothing is deleted in that case.
func (t *target) dropOne(dir string) *Error {
	full := path.Join(t.path, dir)
	for _, m := range t.mounts {
		if m.Point == full || strings.HasPrefix(m.Point, full+"/") {
			return &Error{CodeIO, "another filesystem is mounted inside " + full}
		}
	}
	if fi, err := t.root.Lstat(dir); err != nil || !fi.IsDir() {
		// gone, a file or a symlink: nothing to walk, RemoveAll takes the entry itself
		return t.removeAll(dir)
	}
	err := fs.WalkDir(t.root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi, err := d.Info(); err == nil && devOf(fi) != t.dev {
			return fmt.Errorf("%s is on another filesystem", p)
		}
		return nil
	})
	if err != nil {
		return &Error{CodeIO, err.Error()}
	}
	return t.removeAll(dir)
}

func (t *target) removeAll(dir string) *Error {
	if err := t.root.RemoveAll(dir); err != nil {
		return &Error{CodeIO, err.Error()}
	}
	return nil
}

// walkSize sums the file sizes under dir. A file or folder that vanishes
// during the walk (a live database) counts as nothing.
func walkSize(ctx context.Context, fsys fs.FS, dir string) (n int64, err error) {
	err = fs.WalkDir(fsys, dir, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, err
}
