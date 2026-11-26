package hostdisk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The panel database ops (Kind "paneldb"). Every file operation under
// <DataDir>/db is one of these: the panel's helper never opens db/ through its
// own bind (after the host mount changes that bind is a stale private copy, and
// it would pin the disk). Each run is a fresh helper container, so it sees the
// host's mounts as they are now, and each op that touches db/ reads the mount
// table first and refuses when db/ is not in the state the caller expects.

// PanelDBFolder is the name of the disk folder that holds the database.
const PanelDBFolder = "stackr-panel-db"

// PanelDBMarker is the identification file in the disk folder.
const PanelDBMarker = ".stackr-panel-data"

// Areas an op may name. db is <DataDir>/db, swap is <DataDir>/swap (system
// disk) and folder is the disk folder.
const (
	AreaDB     = "db"
	AreaSwap   = "swap"
	AreaFolder = "folder"
)

// DBFile is one file in an area. The name is a single path element.
type DBFile struct {
	Area string `json:"area"`
	Name string `json:"name"`
}

// DBReq is the paneldb part of a Request.
type DBReq struct {
	DataDir   string `json:"data_dir"`
	DiskID    string `json:"disk_id,omitempty"`
	InstallID string `json:"install_id,omitempty"`
	// Mounted is what db/ must be right now: "yes" or "no". Required by every
	// op that reads or writes db/.
	Mounted  string `json:"mounted,omitempty"`
	From     DBFile `json:"from,omitempty"`
	To       DBFile `json:"to,omitempty"`
	Optional bool   `json:"optional,omitempty"` // rename, rm: a missing source is success
}

// Marker is what .stackr-panel-data holds. Identification, not authentication.
type Marker struct {
	InstallID string `json:"install_id"`
	DiskID    string `json:"disk_id"`
	Time      string `json:"time"`
}

// DBResult is the paneldb part of a Result.
type DBResult struct {
	SHA256   string   `json:"sha256,omitempty"`
	Size     int64    `json:"size,omitempty"`
	Exists   bool     `json:"exists,omitempty"`
	Mounted  bool     `json:"mounted,omitempty"`
	Marker   *Marker  `json:"marker,omitempty"`
	Names    []string `json:"names,omitempty"`
	SysFree  uint64   `json:"sys_free,omitempty"`
	DBBytes  int64    `json:"db_bytes,omitempty"`
	ChattrOK bool     `json:"chattr_ok,omitempty"`
}

// More error codes of this kind.
const (
	CodeMountState = "mount_state" // db/ is (not) a mount point, the other way round than the op expects
	CodeSymlink    = "symlink"     // the data dir path has a symlink on the way
	CodeNoChattr   = "no_chattr"   // chattr +i does not work on the data dir's filesystem
	CodeBadMarker  = "bad_marker"  // the disk folder's marker is missing or not this install's
	CodeHashDiffer = "hash_differ"
)

var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9.][A-Za-z0-9_.-]*$`)

type dbKind struct{}

func (dbKind) run(ctx context.Context, e Env, r Request) Result {
	if r.DB == nil {
		return fail(CodeBadName, "no database request")
	}
	d, herr := e.dbPaths(r)
	if herr != nil {
		return Result{Error: herr}
	}
	switch r.Op {
	case "probe":
		return d.probe(ctx)
	case "place":
		return d.place()
	case "checkpoint":
		return d.sqlOp(ctx, e.Checkpoint, "checkpoint")
	case "integrity":
		return d.sqlOp(ctx, e.Integrity, "integrity check")
	case "copy":
		return d.copy()
	case "hash":
		return d.hash()
	case "rename":
		return d.rename()
	case "rm":
		return d.rm()
	case "list":
		return d.list()
	case "marker-write":
		return d.markerWrite()
	case "marker-read":
		return d.markerRead()
	case "lock":
		return d.lock(ctx, true)
	case "unlock":
		return d.lock(ctx, false)
	case "mount":
		return d.mount(ctx)
	case "unmount":
		return d.unmount(ctx)
	case "clear-underlay":
		return d.clearUnderlay()
	}
	return fail(CodeUnsupported, "op "+r.Op)
}

// dbOp is one validated request.
type dbOp struct {
	e      Env
	r      Request
	q      *DBReq
	data   string // <DataDir> as the host sees it
	db     string // <DataDir>/db, host path
	folder string // the disk folder, host path ("" = none named)
}

func (e Env) dbPaths(r Request) (*dbOp, *Error) {
	q := r.DB
	if !filepath.IsAbs(q.DataDir) || filepath.Clean(q.DataDir) != q.DataDir || q.DataDir == "/" {
		return nil, &Error{CodeBadName, "the data dir must be a full path"}
	}
	d := &dbOp{e: e, r: r, q: q, data: q.DataDir, db: q.DataDir + "/" + AreaDB}
	if q.DiskID != "" {
		if herr := badName(q.DiskID); herr != nil {
			return nil, herr
		}
		d.folder = MountPoint(q.DiskID) + "/" + q.DiskID + "/" + PanelDBFolder
	}
	return d, nil
}

func (d *dbOp) host(p string) string { return joinRoot(d.e.Root, p) }

// canonical refuses a data dir with a symlink on the way: a host path the
// panel container resolves differently would send the files elsewhere.
func (d *dbOp) canonical() *Error {
	canon, err := resolveUnder(d.e.Root, d.data)
	if err != nil {
		return ioOr(err, CodeDiskMissing)
	}
	if canon != d.data {
		return &Error{CodeSymlink, d.data + " is a symlink to " + canon}
	}
	return nil
}

// table is the host's mount table.
func (d *dbOp) table() ([]Mount, *Error) {
	b, err := os.ReadFile(d.e.mountinfo())
	if err != nil {
		return nil, &Error{CodeIO, "read the mount table: " + err.Error()}
	}
	return ParseMounts(b), nil
}

// dbMounted: db/ is a mount point of a device other than its parent's.
func (d *dbOp) dbMounted() (bool, *Error) {
	ms, herr := d.table()
	if herr != nil {
		return false, herr
	}
	m, ok := lastCovering(ms, d.db)
	if !ok || m.Point != d.db {
		return false, nil
	}
	if p, ok := lastCovering(ms, d.data); ok && p.Dev == m.Dev && p.Point != d.db && m.Dev != "" {
		return false, nil
	}
	return true, nil
}

// expect refuses when db/ is not in the state the caller said it must be in.
func (d *dbOp) expect() *Error {
	if d.q.Mounted != "yes" && d.q.Mounted != "no" {
		return &Error{CodeMountState, "the op does not say whether db/ is mounted"}
	}
	got, herr := d.dbMounted()
	if herr != nil {
		return herr
	}
	if got != (d.q.Mounted == "yes") {
		if got {
			return &Error{CodeMountState, d.db + " is a mount point"}
		}
		return &Error{CodeMountState, d.db + " is not a mount point"}
	}
	return nil
}

// dir is the host directory of an area. db/ and the folder are expected to be
// real directories; any area op on db checks the mount state first.
func (d *dbOp) dir(area string) (string, *Error) {
	switch area {
	case AreaDB:
		if herr := d.expect(); herr != nil {
			return "", herr
		}
		return d.host(d.db), nil
	case AreaSwap:
		dir := d.host(d.data + "/swap")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", &Error{CodeIO, err.Error()}
		}
		return dir, nil
	case AreaFolder:
		if d.folder == "" {
			return "", &Error{CodeBadName, "no disk named"}
		}
		if herr := d.diskMounted(); herr != nil {
			return "", herr
		}
		return d.host(d.folder), nil
	}
	return "", &Error{CodeBadName, "area " + area}
}

// diskMounted: the xfs disk is mounted at its point.
func (d *dbOp) diskMounted() *Error {
	ms, herr := d.table()
	if herr != nil {
		return herr
	}
	point := MountPoint(d.q.DiskID)
	m, ok := lastCovering(ms, point)
	if !ok || m.Point != point || m.FSType != "xfs" {
		return &Error{CodeDiskMissing, point + " is not mounted"}
	}
	return nil
}

func (d *dbOp) file(f DBFile) (string, *Error) {
	if !dbNameRe.MatchString(f.Name) || strings.Contains(f.Name, "..") {
		return "", &Error{CodeBadName, "not a file name: " + f.Name}
	}
	dir, herr := d.dir(f.Area)
	if herr != nil {
		return "", herr
	}
	return dir + "/" + f.Name, nil
}

func ok(r *DBResult) Result { return Result{DB: r} }

func (d *dbOp) probe(ctx context.Context) Result {
	if herr := d.canonical(); herr != nil {
		return Result{Error: herr}
	}
	out := &DBResult{}
	var herr *Error
	if out.Mounted, herr = d.dbMounted(); herr != nil {
		return Result{Error: herr}
	}
	if fi, err := os.Lstat(d.host(d.db)); err != nil {
		return Result{Error: ioOr(err, CodeDiskMissing)}
	} else if fi.Mode()&os.ModeSymlink != 0 && !d.e.TempMounts {
		return fail(CodeSymlink, d.db+" is a symlink")
	}
	for _, suf := range []string{"", "-wal"} {
		if fi, err := os.Stat(d.host(d.db + "/stackr.db" + suf)); err == nil {
			out.DBBytes += fi.Size()
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.host(d.data), &st); err != nil {
		return fail(CodeIO, err.Error())
	}
	out.SysFree = st.Bavail * uint64(st.Bsize) //nolint:unconvert // width differs by arch
	// chattr +i must work here: the underlay's whole protection is that flag.
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	scratch := d.host(d.data + "/.stackr-chattr-" + hex.EncodeToString(rnd[:]))
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return fail(CodeIO, err.Error())
	}
	_, err := d.e.exec(ctx, "chattr", "+i", scratch)
	if err == nil {
		out.ChattrOK = true
		_, err = d.e.exec(ctx, "chattr", "-i", scratch)
		if err != nil {
			_ = os.Remove(scratch)
			return fail(CodeIO, "chattr -i on the scratch folder: "+err.Error())
		}
	}
	if err := os.Remove(scratch); err != nil {
		return fail(CodeIO, err.Error())
	}
	return ok(out)
}

// place makes the folder 0700 root: Place() makes disk folders 0755.
func (d *dbOp) place() Result {
	dir, herr := d.dir(AreaFolder)
	if herr != nil {
		return Result{Error: herr}
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return fail(CodeIO, d.folder+" is not a folder")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fail(CodeIO, err.Error())
	}
	if err := os.Chown(dir, 0, 0); err != nil && !d.e.TempMounts {
		return fail(CodeIO, err.Error())
	}
	return ok(&DBResult{})
}

func (d *dbOp) sqlOp(ctx context.Context, fn func(context.Context, string) error, what string) Result {
	if fn == nil {
		return fail(CodeUnsupported, what+" is not available here")
	}
	p, herr := d.file(d.q.From)
	if herr != nil {
		return Result{Error: herr}
	}
	if err := fn(ctx, p); err != nil {
		return fail(CodeIO, what+": "+err.Error())
	}
	return ok(&DBResult{})
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func (d *dbOp) hash() Result {
	p, herr := d.file(d.q.From)
	if herr != nil {
		return Result{Error: herr}
	}
	sum, n, err := hashFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ok(&DBResult{})
	}
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	return ok(&DBResult{SHA256: sum, Size: n, Exists: true})
}

// copy writes From to To through To.partial: fsync, rename, fsync the folder.
// The destination is wiped first, so a retry never trusts an earlier try.
func (d *dbOp) copy() Result {
	src, herr := d.file(d.q.From)
	if herr != nil {
		return Result{Error: herr}
	}
	dst, herr := d.file(d.q.To)
	if herr != nil {
		return Result{Error: herr}
	}
	part := dst + ".partial"
	for _, p := range []string{part, dst} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(CodeIO, err.Error())
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, dst)
	}
	if err == nil {
		err = syncPath(path.Dir(dst))
	}
	if err != nil {
		_ = os.Remove(part)
		return fail(CodeIO, err.Error())
	}
	return ok(&DBResult{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Exists: true})
}

func syncPath(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// rename moves From to To without replacing. A source already gone with the
// destination there is success: that is what a repeated step finds.
func (d *dbOp) rename() Result {
	src, herr := d.file(d.q.From)
	if herr != nil {
		return Result{Error: herr}
	}
	dst, herr := d.file(d.q.To)
	if herr != nil {
		return Result{Error: herr}
	}
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		if d.q.Optional {
			return ok(&DBResult{})
		}
		if _, err := os.Lstat(dst); err == nil {
			return ok(&DBResult{Exists: true})
		}
		return fail(CodeIO, d.q.From.Name+" is not there")
	}
	if _, err := os.Lstat(dst); err == nil {
		return fail(CodeBusy, d.q.To.Name+" already exists")
	}
	// link then unlink: never replaces, and fails with EXDEV across filesystems
	// instead of copying behind our back
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EOPNOTSUPP) {
			if err := os.Rename(src, dst); err != nil {
				return fail(CodeIO, err.Error())
			}
		} else {
			return fail(CodeIO, err.Error())
		}
	} else if err := os.Remove(src); err != nil {
		return fail(CodeIO, err.Error())
	}
	if err := errors.Join(syncPath(path.Dir(src)), syncPath(path.Dir(dst))); err != nil {
		return fail(CodeIO, err.Error())
	}
	return ok(&DBResult{Exists: true})
}

func (d *dbOp) rm() Result {
	p, herr := d.file(d.q.From)
	if herr != nil {
		return Result{Error: herr}
	}
	if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
		return fail(CodeBadName, d.q.From.Name+" is a folder")
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(CodeIO, err.Error())
	}
	return ok(&DBResult{})
}

func (d *dbOp) list() Result {
	dir, herr := d.dir(d.q.From.Area)
	if herr != nil {
		return Result{Error: herr}
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return ok(&DBResult{})
	}
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	out := &DBResult{}
	for _, en := range ents {
		out.Names = append(out.Names, en.Name())
	}
	return ok(out)
}

func (d *dbOp) markerWrite() Result {
	dir, herr := d.dir(AreaFolder)
	if herr != nil {
		return Result{Error: herr}
	}
	b, _ := json.Marshal(Marker{InstallID: d.q.InstallID, DiskID: d.q.DiskID, Time: time.Now().UTC().Format("20060102T150405Z")})
	tmp := dir + "/" + PanelDBMarker + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fail(CodeIO, err.Error())
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err == nil {
		err = os.Rename(tmp, dir+"/"+PanelDBMarker)
	}
	if err == nil {
		err = syncPath(dir)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fail(CodeIO, err.Error())
	}
	return ok(&DBResult{})
}

func readMarker(dir string) (*Marker, error) {
	b, err := os.ReadFile(dir + "/" + PanelDBMarker)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("the marker is unreadable: %w", err)
	}
	return &m, nil
}

func (d *dbOp) markerRead() Result {
	dir, herr := d.dir(d.q.From.Area)
	if herr != nil {
		return Result{Error: herr}
	}
	m, err := readMarker(dir)
	if err != nil {
		return fail(CodeBadMarker, err.Error())
	}
	return ok(&DBResult{Marker: m})
}

func (d *dbOp) lock(ctx context.Context, on bool) Result {
	d.q.Mounted = "no"
	dir, herr := d.dir(AreaDB)
	if herr != nil {
		return Result{Error: herr}
	}
	if herr := d.canonical(); herr != nil {
		return Result{Error: herr}
	}
	if on {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return fail(CodeIO, err.Error())
		}
		if len(ents) > 0 {
			return fail(CodeNotEmpty, d.db+" is not empty")
		}
	}
	flag := "-i"
	if on {
		flag = "+i"
	}
	if _, err := d.e.exec(ctx, "chattr", flag, dir); err != nil {
		return fail(CodeIO, err.Error())
	}
	return ok(&DBResult{})
}

// bindUnit is the mount unit that puts the disk folder on <DataDir>/db: the
// same managed header and exact-match rule as the xfs unit, RequiresMountsFor
// both ends, before Docker, nofail, wanted by local-fs.target.
func bindUnit(diskPoint, folder, where string) string {
	return "# managed by stackr\n[Unit]\nDescription=stackr panel database\nRequiresMountsFor=" + diskPoint + " " + where + "\nBefore=docker.service\n\n" +
		"[Mount]\nWhat=" + folder + "\nWhere=" + where + "\nType=none\nOptions=bind,nofail\n\n" +
		"[Install]\nWantedBy=local-fs.target\n"
}

// BindUnitName is the unit that mounts the database folder of dataDir.
func BindUnitName(dataDir string) string { return unitName(dataDir + "/" + AreaDB) }

// unmounted underlay holds nothing and is locked; the folder holds this
// install's marker. Then the unit is written and started.
func (d *dbOp) mount(ctx context.Context) Result {
	if herr := d.canonical(); herr != nil {
		return Result{Error: herr}
	}
	if herr := d.diskMounted(); herr != nil {
		return Result{Error: herr}
	}
	folder := d.host(d.folder)
	fi, err := os.Stat(folder)
	if err != nil || !fi.IsDir() {
		return fail(CodeDiskMissing, d.folder+" is not there")
	}
	if fi.Mode().Perm() != 0o700 {
		return fail(CodeBadMarker, d.folder+" is not 0700")
	}
	m, err := readMarker(folder)
	if err != nil || m == nil || m.InstallID != d.q.InstallID || m.DiskID != d.q.DiskID {
		return fail(CodeBadMarker, "the disk folder has no marker of this install")
	}
	mounted, herr := d.dbMounted()
	if herr != nil {
		return Result{Error: herr}
	}
	if !mounted {
		d.q.Mounted = "no"
		if res := d.lock(ctx, true); res.Error != nil {
			return res
		}
	}
	if herr := d.writeUnitText(BindUnitName(d.data), bindUnit(MountPoint(d.q.DiskID), d.folder, d.db)); herr != nil {
		return Result{Error: herr}
	}
	unit := BindUnitName(d.data)
	for _, args := range [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", unit}} {
		if _, err := d.e.host(ctx, args...); err != nil {
			return fail(CodeIO, "enable the bind unit: "+err.Error())
		}
	}
	if !mounted {
		if _, err := d.e.host(ctx, "systemctl", "start", unit); err != nil {
			return fail(CodeIO, "start the bind unit: "+err.Error())
		}
	}
	d.q.Mounted = "yes"
	if herr := d.expect(); herr != nil {
		return Result{Error: herr}
	}
	// The marker is not read here: a mount made by this run is not visible in
	// this container's view of /host (xfs.go: a fresh run sees it). The mover
	// reads the marker and the hash in the next run.
	return ok(&DBResult{Mounted: true})
}

func (d *dbOp) writeUnitText(name, text string) *Error { return d.e.writeUnitText(name, text) }

// unmount stops our unit and removes it. Idempotent. A mount that is there
// with no unit of ours behind it is not ours to unmount.
func (d *dbOp) unmount(ctx context.Context) Result {
	unit := BindUnitName(d.data)
	unitFile := d.host("/etc/systemd/system/" + unit)
	have := false
	if b, err := os.ReadFile(unitFile); err == nil {
		if string(b) != bindUnit(MountPoint(d.q.DiskID), d.folder, d.db) {
			return fail(CodeBusy, unit+" is not the stackr bind unit of this disk")
		}
		have = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(CodeIO, err.Error())
	}
	mounted, herr := d.dbMounted()
	if herr != nil {
		return Result{Error: herr}
	}
	if mounted {
		if !have {
			return fail(CodeBusy, d.db+" is mounted by something that is not the stackr unit")
		}
		if _, err := d.e.host(ctx, "systemctl", "stop", unit); err != nil {
			return fail(CodeBusy, err.Error())
		}
		if still, herr := d.dbMounted(); herr != nil {
			return Result{Error: herr}
		} else if still {
			return fail(CodeBusy, d.db+" is still mounted")
		}
	}
	if have {
		if _, err := d.e.host(ctx, "systemctl", "disable", unit); err != nil {
			return fail(CodeIO, "disable the bind unit: "+err.Error())
		}
		if err := os.Remove(unitFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(CodeIO, err.Error())
		}
		if _, err := d.e.host(ctx, "systemctl", "daemon-reload"); err != nil {
			return fail(CodeIO, "reload systemd: "+err.Error())
		}
	}
	return ok(&DBResult{})
}

// clearUnderlay removes the database files from an unmounted db/. The caller
// has unlocked it: an immutable folder refuses the removal.
func (d *dbOp) clearUnderlay() Result {
	d.q.Mounted = "no"
	dir, herr := d.dir(AreaDB)
	if herr != nil {
		return Result{Error: herr}
	}
	for _, n := range []string{"stackr.db", "stackr.db-wal", "stackr.db-shm"} {
		if err := os.Remove(dir + "/" + n); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(CodeIO, err.Error())
		}
	}
	return ok(&DBResult{})
}
