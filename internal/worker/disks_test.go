package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
)

const (
	rootLine = "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n"
	dataLine = "2 1 8:16 / /mnt/data rw - ext4 /dev/sdb1 rw\n"
	nfsLine  = "3 1 0:40 / /mnt/nas rw - nfs4 nas:/x rw\n"
)

type readFn = func(ctx context.Context, path string) (hostdisk.Stamp, uint64, uint64, error)

// hang reads until the refresher's context ends.
func hang(ctx context.Context, _ string) (hostdisk.Stamp, uint64, uint64, error) {
	<-ctx.Done()
	return hostdisk.Stamp{}, 0, 0, ctx.Err()
}

// diskServer is a worker whose host root holds a mount table; reads are fake.
func diskServer(t *testing.T, table string, read readFn) *Server {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc/1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/1/mountinfo"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{StateDir: t.TempDir(), HostRoot: root, DockerSock: "/nonexistent"})
	t.Cleanup(s.Stop)
	s.diskEvery, s.diskTimeout = time.Hour, time.Hour
	if read != nil {
		s.readDisk = read
	}
	return s
}

func facts(s *Server) Facts { var f Facts; s.diskFacts(&f); return f }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

var regA = DiskReg{ID: "d1", Path: "/mnt/data", Owner: "n1"}

func TestDisksFactsParseTheTableAndNeverOpenAMount(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	s := diskServer(t, rootLine+dataLine+nfsLine, func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
		<-block // a blocked filesystem
		return hostdisk.Stamp{}, 0, 0, nil
	})
	if err := s.PutDisks([]DiskReg{regA, {ID: "d2", Path: "/mnt/nas", Owner: "n1"}, {ID: "d3", Path: "/mnt/gone", Owner: "n1"}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	f := s.Facts(context.Background())
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Facts waited on the filesystem: %v", time.Since(start))
	}
	if len(f.Disks) != 3 || !f.Disks[0].Mounted || f.Disks[0].RootFS || f.Disks[0].FSType != "ext4" ||
		!f.Disks[1].Network || f.Disks[2].Mounted {
		t.Fatalf("table facts wrong: %+v", f.Disks)
	}
	if !f.DisksStale {
		t.Error("never-read paths must report stale")
	}
}

func TestDisksFactsServeTheCachedStamp(t *testing.T) {
	var n atomic.Int32
	s := diskServer(t, rootLine+dataLine, func(ctx context.Context, p string) (hostdisk.Stamp, uint64, uint64, error) {
		if n.Add(1) == 1 {
			return hostdisk.Stamp{ID: "d1", Owner: "n1", Install: "i"}, 5, 10, nil
		}
		return hang(ctx, p) // later reads hang
	})
	s.diskEvery, s.diskTimeout = 40*time.Millisecond, 40*time.Millisecond
	if err := s.PutDisks([]DiskReg{regA}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cached stamp", func() bool { return facts(s).Disks[0].StampID == "d1" })
	if d := facts(s).Disks[0]; d.StampOwner != "n1" || d.StampInstall != "i" || d.Free != 5 || d.Total != 10 {
		t.Fatalf("cached fact: %+v", d)
	}
	eventually(t, "stale past the timeout", func() bool { return facts(s).DisksStale })
	if facts(s).Disks[0].StampID != "d1" {
		t.Error("a stale read must still serve the last value")
	}
}

func TestDisksRegistryPersistsAndReplaysAtStart(t *testing.T) {
	s := diskServer(t, rootLine, hang)
	regs := []DiskReg{regA}
	if err := s.PutDisks(regs); err != nil {
		t.Fatal(err)
	}
	s2 := New(Config{StateDir: s.cfg.StateDir, HostRoot: s.cfg.HostRoot})
	t.Cleanup(s2.Stop)
	s2.readDisk = hang
	if len(facts(s2).DiskRegs) != 0 {
		t.Fatal("registry must be empty before replay")
	}
	s2.ReplayDisks()
	if got := facts(s2).DiskRegs; !slices.Equal(got, regs) || s2.disks.gen != 1 {
		t.Fatalf("replayed %v gen %d", got, s2.disks.gen)
	}
}

func TestDisksUnregisteredMountsAreNotReported(t *testing.T) {
	s := diskServer(t, rootLine+dataLine+nfsLine, hang)
	if err := s.PutDisks([]DiskReg{regA}); err != nil {
		t.Fatal(err)
	}
	if f := facts(s); len(f.Disks) != 1 || f.Disks[0].Path != "/mnt/data" {
		t.Fatalf("reported %+v", f.Disks)
	}
}

func TestDisksReadAndEmptyList(t *testing.T) {
	s := diskServer(t, rootLine, nil)
	f := s.Facts(context.Background())
	if !f.DisksRead || f.Disks == nil || len(f.Disks) != 0 || f.DisksStale {
		t.Fatalf("no registry: %+v", f)
	}
}

func TestDisksPutValidatesThenPersistsThenSwapsUnderTheLock(t *testing.T) {
	s := diskServer(t, rootLine, hang)
	for _, bad := range [][]DiskReg{
		{{ID: "d1", Path: "rel", Owner: "n"}},
		{{ID: "d1", Path: "/a/../b", Owner: "n"}},
		{{ID: "d1", Path: "/a", Owner: "n"}, {ID: "d1", Path: "/b", Owner: "n"}},
		{{ID: "d1", Path: "/a", Owner: "n"}, {ID: "d2", Path: "/a", Owner: "n"}},
		{{ID: "", Path: "/a", Owner: "n"}},
	} {
		if s.PutDisks(bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := os.Stat(s.disksPath()); err == nil || len(facts(s).DiskRegs) != 0 {
		t.Fatal("a rejected list must not persist or swap")
	}
	s.mu.Lock() // a put waits for the server lock
	done := make(chan error)
	go func() { done <- s.PutDisks([]DiskReg{regA}) }()
	select {
	case <-done:
		t.Fatal("put ran without the server lock")
	case <-time.After(50 * time.Millisecond):
	}
	s.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(s.disksPath()); !strings.Contains(string(b), `"generation":1`) || len(facts(s).DiskRegs) != 1 {
		t.Fatalf("persisted %s", b)
	}
}

func TestDisksPersistFailureChangesNothingInMemory(t *testing.T) {
	s := diskServer(t, rootLine, hang)
	if err := s.PutDisks([]DiskReg{regA}); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.StateDir = filepath.Join(blocker, "sub") // MkdirAll fails
	if s.PutDisks([]DiskReg{{ID: "d9", Path: "/mnt/other", Owner: "n1"}}) == nil {
		t.Fatal("persist failure must be an error")
	}
	if got := facts(s).DiskRegs; !slices.Equal(got, []DiskReg{regA}) || s.disks.gen != 1 {
		t.Fatalf("memory changed: %v gen %d", got, s.disks.gen)
	}
}

// DL: a read of a forgotten path that finishes late must never reach the cache.
func TestDisksGenerationDiscardsALateRefreshOfARemovedPath(t *testing.T) {
	var n atomic.Int32
	gate := make(chan struct{})
	started := make(chan struct{})
	s := diskServer(t, rootLine+dataLine, func(ctx context.Context, p string) (hostdisk.Stamp, uint64, uint64, error) {
		if n.Add(1) == 1 {
			close(started)
			<-gate // a read stuck in the kernel, ignoring ctx
			return hostdisk.Stamp{ID: "OLD"}, 1, 1, nil
		}
		return hang(ctx, p)
	})
	if err := s.PutDisks([]DiskReg{regA}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := s.PutDisks(nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDisks([]DiskReg{regA}); err != nil { // adopted again: a new entry
		t.Fatal(err)
	}
	eventually(t, "the new entry reads", func() bool { return n.Load() >= 2 })
	close(gate)
	time.Sleep(50 * time.Millisecond)
	if d := facts(s).Disks[0]; d.StampID != "" {
		t.Fatalf("a late result of a removed path reached the cache: %+v", d)
	}
}

func TestDisksRemovalCancelsTheRefresher(t *testing.T) {
	got := make(chan context.Context, 1)
	s := diskServer(t, rootLine, func(ctx context.Context, p string) (hostdisk.Stamp, uint64, uint64, error) {
		select {
		case got <- ctx:
		default:
		}
		return hang(ctx, p)
	})
	if err := s.PutDisks([]DiskReg{regA}); err != nil {
		t.Fatal(err)
	}
	ctx := <-got
	if err := s.PutDisks(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the refresher kept running after its path left the list")
	}
}

// DL: a blocked read starts no second read, and costs one goroutine, not one per tick.
func TestDisksRefreshIsSingleFlightAndABlockedOneLeaksOneGoroutine(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	s := diskServer(t, rootLine+dataLine, func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
		calls.Add(1)
		<-block // ignores the deadline
		return hostdisk.Stamp{}, 0, 0, nil
	})
	s.diskEvery, s.diskTimeout = 5*time.Millisecond, 5*time.Millisecond
	if err := s.PutDisks([]DiskReg{regA}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first read", func() bool { return calls.Load() == 1 })
	time.Sleep(150 * time.Millisecond) // about 30 ticks
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d reads started while the first was blocked", n)
	}
	if !facts(s).DisksStale {
		t.Error("a blocked read past its timeout must mark the path stale")
	}
}

func TestDisksPutRouteAndFactsEchoTheRegistry(t *testing.T) {
	s := diskServer(t, rootLine+dataLine, hang)
	h := s.Handler()
	put := func(body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/disks", strings.NewReader(body)))
		return rec.Code
	}
	if c := put(`[{"id":"d1","path":"/mnt/data","owner":"n1"}]`); c != 204 {
		t.Fatalf("put: %d", c)
	}
	if c := put(`[{"id":"d1","path":"data","owner":"n1"}]`); c != 400 {
		t.Fatalf("bad path: %d", c)
	}
	if c := put(`nope`); c != 400 {
		t.Fatalf("bad json: %d", c)
	}
	if f := s.Facts(context.Background()); !slices.Equal(f.DiskRegs, []DiskReg{regA}) {
		t.Fatalf("echo %v", f.DiskRegs)
	}
}

// H8: only a real answer is a read; an I/O or stamp failure leaves the path stale.
func TestDisksReadOnlyARealAnswerCountsAsARead(t *testing.T) {
	root := t.TempDir()
	mnt := filepath.Join(root, "mnt/data")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "proc/1"), 0o755); err != nil {
		t.Fatal(err)
	}
	dev, err := hostdisk.DevString(mnt)
	if err != nil {
		t.Fatal(err)
	}
	table := rootLine + "2 1 " + dev + " / /mnt/data rw - ext4 /dev/sdb1 rw\n"
	if err := os.WriteFile(filepath.Join(root, "proc/1/mountinfo"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{StateDir: t.TempDir(), HostRoot: root, DockerSock: "/nonexistent"})
	t.Cleanup(s.Stop)
	read := func() error { _, _, _, err := s.readDiskDefault(context.Background(), "/mnt/data"); return err }
	if err := read(); err != nil { // mounted, no stamp: a read with an empty stamp
		t.Fatalf("no stamp: %v", err)
	}
	stamp := filepath.Join(mnt, ".stackr-disk")
	if err := os.WriteFile(stamp, []byte(`{"id":"d1","owner":"n1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _, _, err := s.readDiskDefault(context.Background(), "/mnt/data"); err != nil || st.ID != "d1" {
		t.Fatalf("stamp: %+v %v", st, err)
	}
	if err := os.WriteFile(stamp, nil, 0o644); err != nil { // broken stamp
		t.Fatal(err)
	}
	if read() == nil {
		t.Error("a broken stamp must not count as a read")
	}
	if err := os.Remove(stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stamp, 0o755); err != nil { // read fails with an I/O error
		t.Fatal(err)
	}
	if read() == nil {
		t.Error("an I/O error must not count as a read")
	}
	// a typed io refusal (no mount table) is not a read either
	if err := os.Remove(filepath.Join(root, "proc/1/mountinfo")); err != nil {
		t.Fatal(err)
	}
	if read() == nil {
		t.Error("a typed io error must not count as a read")
	}
	// not the disk at all: a real answer, an empty stamp
	if err := os.WriteFile(filepath.Join(root, "proc/1/mountinfo"), []byte(rootLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _, _, err := s.readDiskDefault(context.Background(), "/mnt/data"); err != nil || st.ID != "" {
		t.Fatalf("unmounted: %+v %v", st, err)
	}
}

// H9: a fact carries the time of the read it came from.
func TestDisksFactsCarryTheReadTime(t *testing.T) {
	s := diskServer(t, rootLine+dataLine, func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
		return hostdisk.Stamp{ID: "d1"}, 1, 2, nil
	})
	before := time.Now()
	if err := s.PutDisks([]DiskReg{regA}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first read", func() bool { return !facts(s).Disks[0].ReadAt.IsZero() })
	if at := facts(s).Disks[0].ReadAt; at.Before(before) {
		t.Fatalf("read at %v before the read started %v", at, before)
	}
}
