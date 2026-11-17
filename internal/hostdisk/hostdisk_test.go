package hostdisk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rootLine = "25 1 8:1 / / rw - ext4 /dev/sda1 rw\n"

// rig: Root is a temp dir, /disk is a directory in it, and the mount table
// (written by the test) says whether /disk is a mount point.
type rig struct {
	t    *testing.T
	env  Env
	disk string // the real dir behind /disk
}

func newRig(t *testing.T, extra string) *rig {
	t.Helper()
	root := t.TempDir()
	disk := filepath.Join(root, "disk")
	if err := os.Mkdir(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, env: Env{Root: root, Mountinfo: filepath.Join(t.TempDir(), "mountinfo")}, disk: disk}
	r.table(rootLine + extra)
	return r
}

func (r *rig) table(s string) {
	r.t.Helper()
	if err := os.WriteFile(r.env.Mountinfo, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

const diskLine = "36 25 8:17 / /disk rw - ext4 /dev/sdb rw\n"

func (r *rig) run(op string, mod func(*Request)) Result {
	req := Request{Op: op, Kind: "path", Mount: "/disk", DiskID: "d1", Owner: "n1", Install: "i1"}
	if mod != nil {
		mod(&req)
	}
	return Run(context.Background(), r.env, req)
}

func (r *rig) mkdir(p string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Join(r.disk, p), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) write(p, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.disk, p), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) exists(p string) bool {
	_, err := os.Lstat(filepath.Join(r.disk, p))
	return err == nil
}

func wantCode(t *testing.T, res Result, code string) {
	t.Helper()
	if code == "" {
		if res.Error != nil {
			t.Fatalf("want ok, got %v", res.Error)
		}
		return
	}
	if res.Error == nil || res.Error.Code != code {
		t.Fatalf("want %s, got %v", code, res.Error)
	}
}

// added is a rig with /disk mounted and stamped by add.
func added(t *testing.T) *rig {
	r := newRig(t, diskLine)
	wantCode(t, r.run("add", nil), "")
	return r
}

func TestProbePlainFolderRefused(t *testing.T) {
	r := newRig(t, "")
	wantCode(t, r.run("probe", nil), CodeNotMountPoint)
}

func TestProbeRootFilesystemRefused(t *testing.T) {
	r := newRig(t, "36 25 8:1 /sub /disk rw - ext4 /dev/sda1 rw\n")
	wantCode(t, r.run("probe", nil), CodeRootFS)
}

func TestProbeNonemptyWithoutStampRefused(t *testing.T) { // DL
	r := newRig(t, diskLine)
	r.write("data.txt", "x")
	wantCode(t, r.run("probe", nil), CodeNotEmpty)
	wantCode(t, r.run("add", nil), CodeNotEmpty)
	if r.exists(stampFile) {
		t.Fatal("add stamped a non-empty disk")
	}
}

func TestProbeLostFoundCountsAsEmpty(t *testing.T) {
	r := newRig(t, diskLine)
	r.mkdir("lost+found")
	res := r.run("probe", nil)
	wantCode(t, res, "")
	if !res.Disk.Empty || !res.Disk.Mounted || res.Disk.FSType != "ext4" {
		t.Fatalf("fact %+v", res.Disk)
	}
}

func TestProbeNFSAndCIFSWarnNotRefuse(t *testing.T) {
	for _, fs := range []string{"nfs4", "cifs"} {
		r := newRig(t, "36 25 0:40 / /disk rw - "+fs+" srv:/x rw\n")
		res := r.run("probe", nil)
		wantCode(t, res, "")
		if !res.Disk.Network {
			t.Fatalf("%s not flagged as network", fs)
		}
	}
}

func TestProbeForeignStampReported(t *testing.T) {
	r := newRig(t, diskLine)
	r.write(stampFile, `{"id":"other","install":"x","kind":"path","owner":"n9"}`)
	res := r.run("probe", nil)
	wantCode(t, res, "")
	if res.Disk.StampID != "other" || res.Disk.StampOwner != "n9" || res.Disk.StampInstall != "x" {
		t.Fatalf("fact %+v", res.Disk)
	}
}

func TestAddWritesStampOnceAndCreatesTheDiskIDFolder(t *testing.T) {
	r := added(t)
	s, err := ReadStamp(context.Background(), r.env, "/disk")
	if err != nil || s.ID != "d1" || s.Owner != "n1" || s.Install != "i1" || s.Kind != "path" || s.Created.IsZero() {
		t.Fatalf("stamp %+v %v", s, err)
	}
	if !r.exists("d1") {
		t.Fatal("no id folder")
	}
	before, _ := os.ReadFile(filepath.Join(r.disk, stampFile))
	wantCode(t, r.run("add", nil), "") // a retry changes nothing
	after, _ := os.ReadFile(filepath.Join(r.disk, stampFile))
	if string(before) != string(after) {
		t.Fatal("stamp rewritten")
	}
	wantCode(t, r.run("add", func(q *Request) { q.DiskID = "d2" }), CodeForeignStamp)
	wantCode(t, r.run("add", func(q *Request) { q.Owner = "n2" }), CodeForeignStamp)
}

func TestVerifyUnmountedIsDiskMissing(t *testing.T) { // DL
	r := added(t)
	wantCode(t, r.run("verify", nil), "")
	r.table("") // unmounted
	wantCode(t, r.run("verify", nil), CodeDiskMissing)
}

func TestVerifyWrongStampIsDiskMissing(t *testing.T) { // DL
	r := added(t)
	wantCode(t, r.run("verify", func(q *Request) { q.DiskID = "d2" }), CodeDiskMissing)
	if err := os.Remove(filepath.Join(r.disk, stampFile)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("verify", nil), CodeDiskMissing)
}

func TestVerifyWrongOwnerIsForeignStamp(t *testing.T) { // DL
	r := added(t)
	wantCode(t, r.run("verify", func(q *Request) { q.Owner = "n2" }), CodeForeignStamp)
}

func TestFolderIsSingleLevelMkdirAndNeverCreatesAMissingParent(t *testing.T) { // DL
	r := added(t)
	wantCode(t, r.run("folder", func(q *Request) { q.Names = []string{"vol1", "vol2"} }), "")
	if !r.exists("d1/vol1") || !r.exists("d1/vol2") {
		t.Fatal("folders missing")
	}
	wantCode(t, r.run("folder", func(q *Request) { q.Names = []string{"vol1"} }), "") // idempotent
	if err := os.Remove(filepath.Join(r.disk, "d1", "vol1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.disk, "d1", "vol2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.disk, "d1")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("folder", func(q *Request) { q.Names = []string{"vol1"} }), CodeDiskMissing)
	if r.exists("d1") {
		t.Fatal("the id folder was created by folder")
	}
}

func TestFolderAfterUnmountCreatesNothingOnTheRootFilesystem(t *testing.T) { // DL
	r := added(t)
	r.table("")
	wantCode(t, r.run("folder", func(q *Request) { q.Names = []string{"vol1"} }), CodeDiskMissing)
	if r.exists("d1/vol1") {
		t.Fatal("created under the unmounted path")
	}
	// the root handle opened before an unmount keeps pointing at the disk:
	// an absent mount point never reaches a mkdir at all
	if _, err := os.Stat(filepath.Join(r.env.Root, "d1")); err == nil {
		t.Fatal("created on the root")
	}
}

func TestStampAndFolderGoThroughTheRootHandleAndSymlinksCannotEscape(t *testing.T) { // DL
	r := added(t)
	outside := t.TempDir()
	// a symlink inside the disk pointing out must not be followed by folder or drop
	if err := os.Symlink(outside, filepath.Join(r.disk, "d1", "esc")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("drop", func(q *Request) { q.Names = []string{"esc"} }), "")
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("drop followed a symlink out of the disk")
	}
	// a stamp that is a symlink out of the disk is not read
	if err := os.Remove(filepath.Join(r.disk, stampFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "stamp"), []byte(`{"id":"d1","owner":"n1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "stamp"), filepath.Join(r.disk, stampFile)); err != nil {
		t.Fatal(err)
	}
	if res := r.run("verify", nil); res.Error == nil {
		t.Fatal("verify read a stamp outside the disk")
	}
	// a symlinked mount path under Root resolves inside Root: an absolute link
	// to /disk lands on Root/disk, not on the real /disk of the machine
	r = newRig(t, diskLine)
	if err := os.Symlink("/disk", filepath.Join(r.env.Root, "alias")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("probe", func(q *Request) { q.Mount = "/alias" }), "")
	// a link to / is the root filesystem, never a disk
	if err := os.Symlink("/", filepath.Join(r.env.Root, "up")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("probe", func(q *Request) { q.Mount = "/up" }), CodeRootFS)
}

func TestDropVerifiesBeforeRemove(t *testing.T) { // DL
	r := added(t)
	wantCode(t, r.run("folder", func(q *Request) { q.Names = []string{"vol1"} }), "")
	r.write("d1/vol1/f", "x")
	r.table("") // unmounted
	wantCode(t, r.run("drop", func(q *Request) { q.Names = []string{"vol1"} }), CodeDiskMissing)
	r.table(rootLine + diskLine)
	wantCode(t, r.run("drop", func(q *Request) { q.Names = []string{"vol1"}; q.Owner = "n2" }), CodeForeignStamp)
	if !r.exists("d1/vol1/f") {
		t.Fatal("drop removed data without a verified stamp")
	}
	wantCode(t, r.run("drop", func(q *Request) { q.Names = []string{"vol1"} }), "")
	if r.exists("d1/vol1") {
		t.Fatal("not dropped")
	}
}

func TestDropRejectsBadNames(t *testing.T) { // DL
	r := added(t)
	r.mkdir("d1/vol1")
	for _, n := range []string{"..", "a/b", "", ".", "/abs", ".hidden", "-x"} {
		wantCode(t, r.run("drop", func(q *Request) { q.Names = []string{"vol1", n} }), CodeBadName)
	}
	if !r.exists("d1/vol1") {
		t.Fatal("a bad name in the list still dropped the good one")
	}
	wantCode(t, r.run("drop", func(q *Request) { q.DiskID = ".." }), CodeBadName)
}

func TestDropLeavesSiblingFolders(t *testing.T) {
	r := added(t)
	r.mkdir("d1/a")
	r.mkdir("d1/b")
	r.write("d1/b/f", "x")
	wantCode(t, r.run("drop", func(q *Request) { q.Names = []string{"a"} }), "")
	if r.exists("d1/a") || !r.exists("d1/b/f") || !r.exists(stampFile) {
		t.Fatal("drop touched more than its name")
	}
}

func TestUsageEmptyVsNonemptyWithoutWalk(t *testing.T) {
	r := added(t)
	r.mkdir("d1/empty")
	r.mkdir("d1/full")
	r.write("d1/full/f", "12345")
	res := r.run("usage", func(q *Request) { q.Names = []string{"empty", "full"} })
	wantCode(t, res, "")
	if !res.Usage["empty"].Empty || res.Usage["full"].Empty || res.Usage["full"].Bytes != 0 {
		t.Fatalf("%+v", res.Usage)
	}
	res = r.run("usage", func(q *Request) { q.Names = []string{"full"}; q.Sizes = true })
	if res.Usage["full"].Bytes != 5 {
		t.Fatalf("%+v", res.Usage)
	}
}

func TestUsageMissingFolder(t *testing.T) {
	r := added(t)
	res := r.run("usage", func(q *Request) { q.Names = []string{"nope"}; q.Sizes = true })
	wantCode(t, res, "")
	if !res.Usage["nope"].Missing {
		t.Fatalf("%+v", res.Usage)
	}
}

func TestMountinfoOctalEscapes(t *testing.T) {
	ms := ParseMounts([]byte("36 25 8:17 / /mnt/my\\040disk rw,x shared:1 - ext4 /dev/sd\\134b rw\n"))
	if len(ms) != 1 || ms[0].Point != "/mnt/my disk" || ms[0].Source != `/dev/sd\b` || ms[0].FSType != "ext4" {
		t.Fatalf("%+v", ms)
	}
}

func TestMountinfoStackedMountsTakeTheLast(t *testing.T) {
	ms := ParseMounts([]byte(rootLine + "36 25 8:17 / /disk rw - ext4 a rw\n37 25 8:33 / /disk rw - xfs b rw\n"))
	m, ok := lastCovering(ms, "/disk")
	if !ok || m.FSType != "xfs" {
		t.Fatalf("%+v", m)
	}
	if m, _ := lastCovering(ms, "/disk/sub"); m.FSType != "xfs" || m.Point != "/disk" {
		t.Fatalf("%+v", m)
	}
}

func TestMountPathIsCanonicalised(t *testing.T) {
	r := newRig(t, diskLine)
	if err := os.Symlink("disk", filepath.Join(r.env.Root, "rel")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("probe", func(q *Request) { q.Mount = "/rel/../disk/./" }), "")
	wantCode(t, r.run("probe", func(q *Request) { q.Mount = "disk" }), CodeNotMountPoint)
}

func TestParseMountsTouchesNoFilesystem(t *testing.T) {
	// bytes in, mounts out: nothing here names a file, and junk is skipped
	if ms := ParseMounts([]byte("junk\n\n1 2 3\n")); len(ms) != 0 {
		t.Fatalf("%+v", ms)
	}
}

func TestResultLineEncodeAndDecodeWithStderrNoise(t *testing.T) {
	want := Result{Error: &Error{CodeNotEmpty, "x"}, Disk: &Fact{Path: "/d", Free: 7}, Usage: map[string]Use{"a": {Bytes: 3}}}
	out := "level=warn noise\n" + ResultPrefix + `{"Error":null}` + "\nmore noise\n" + EncodeResult(want) + "\ntrailing stderr\n"
	got, ok := ParseResult([]byte(out))
	if !ok || got.Error == nil || got.Error.Code != CodeNotEmpty || got.Disk.Free != 7 || got.Usage["a"].Bytes != 3 {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := ParseResult([]byte("only noise\n")); ok {
		t.Fatal("a result from nothing")
	}
	req := Request{Op: "folder", Kind: "path", Names: []string{"a"}}
	back, err := DecodeRequest(EncodeRequest(req))
	if err != nil || back.Op != "folder" || strings.Join(back.Names, ",") != "a" {
		t.Fatalf("%+v %v", back, err)
	}
}

func TestUnsupportedKindAndOp(t *testing.T) {
	r := newRig(t, diskLine)
	wantCode(t, r.run("probe", func(q *Request) { q.Kind = "lvm" }), CodeUnsupported)
	wantCode(t, r.run("grow", nil), CodeUnsupported)
}
