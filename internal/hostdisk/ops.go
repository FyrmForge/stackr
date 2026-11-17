package hostdisk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"syscall"
	"time"
)

const stampFile = ".stackr-disk"

// target is a verified mount point, opened once. Every read and write of the
// disk goes through root, so a symlink or an unmount after the open can never
// reach the root filesystem.
type target struct {
	root  *os.Root
	mount Mount
	path  string // canonical, as the host sees it
	disk  string // the path as the caller gave it
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
		return nil, &Error{missing, path + ": " + err.Error()}
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
		return nil, &Error{missing, err.Error()}
	}
	return &target{root: root, mount: m, path: canon, disk: path}, nil
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
	return s, true, json.Unmarshal(b, &s)
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
		if d.Name() != "lost+found" {
			return false, nil
		}
	}
	return true, nil
}

func (t *target) fact(s Stamp, empty bool) *Fact {
	f := &Fact{Path: t.disk, StampID: s.ID, StampOwner: s.Owner, StampInstall: s.Install,
		FSType: t.mount.FSType, Mounted: true, Network: isNetwork(t.mount.FSType), Empty: empty}
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
	s, found, err := readStamp(t.root)
	if err != nil {
		return fail(CodeForeignStamp, "the stamp is unreadable: "+err.Error())
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
	s, found, err := readStamp(t.root)
	switch {
	case err != nil:
		return fail(CodeForeignStamp, "the stamp is unreadable: "+err.Error())
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
	return Result{Disk: t.fact(s, false)}
}

func writeStamp(root *os.Root, s Stamp) error {
	b, _ := json.Marshal(s)
	f, err := root.OpenFile(stampFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
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
	if err != nil {
		_ = root.Remove(stampFile)
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
		return nil, s, &Error{CodeForeignStamp, "the stamp is unreadable: " + err.Error()}
	case !found || s.ID != r.DiskID:
		t.close()
		return nil, s, &Error{CodeDiskMissing, "the disk is not the one registered"}
	case s.Owner != r.Owner:
		t.close()
		return nil, s, &Error{CodeForeignStamp, "the disk belongs to another node"}
	}
	if fi, err := t.root.Stat(r.DiskID); err != nil || !fi.IsDir() {
		t.close()
		return nil, s, &Error{CodeDiskMissing, "the disk has no folder for this id"}
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
		if err := t.root.Mkdir(r.DiskID+"/"+n, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			if errors.Is(err, fs.ErrNotExist) {
				return fail(CodeDiskMissing, "the disk has no folder for this id")
			}
			return fail(CodeIO, err.Error())
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
			err := fs.WalkDir(fsys, dir, func(_ string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				if !d.IsDir() {
					if fi, err := d.Info(); err == nil {
						u.Bytes += fi.Size()
					}
				}
				return nil
			})
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
		if err := t.root.RemoveAll(r.DiskID + "/" + n); err != nil {
			return fail(CodeIO, err.Error())
		}
	}
	return Result{}
}
