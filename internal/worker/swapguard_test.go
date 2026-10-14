package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func swapBin(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for _, n := range SwapBinaries {
		for suffix, body := range map[string]string{"": "new", ".prev": "old"} {
			if err := os.WriteFile(filepath.Join(bin, n+suffix), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return bin
}

// A swap that is pending past its deadline (Docker or the host died, the
// helper never restored) is rolled back at the next worker start.
func TestRestoreStaleSwap(t *testing.T) {
	bin := swapBin(t)
	if err := SwapPending(bin, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	ok, err := RestoreStaleSwap(bin, time.Now())
	if err != nil || !ok {
		t.Fatalf("restored = %v, %v; want true", ok, err)
	}
	if b, _ := os.ReadFile(filepath.Join(bin, "stackrd")); string(b) != "old" {
		t.Fatalf("stackrd = %q, want the old one", b)
	}
	if ok, _ := RestoreStaleSwap(bin, time.Now()); ok {
		t.Fatal("restored twice")
	}
}

// Inside the deadline the helper is still waiting; a committed swap and a
// host with no swap state at all are left alone.
func TestRestoreStaleSwapLeavesOthers(t *testing.T) {
	bin := swapBin(t)
	if ok, _ := RestoreStaleSwap(bin, time.Now()); ok {
		t.Fatal("restored with no swap state")
	}
	_ = SwapPending(bin, time.Now().Add(time.Minute))
	if ok, _ := RestoreStaleSwap(bin, time.Now()); ok {
		t.Fatal("restored inside the deadline")
	}
	if err := SwapCommit(bin); err != nil {
		t.Fatal(err)
	}
	if ok, _ := RestoreStaleSwap(bin, time.Now().Add(time.Hour)); ok {
		t.Fatal("restored a committed swap")
	}
	if b, _ := os.ReadFile(filepath.Join(bin, "stackrd")); string(b) != "new" {
		t.Fatalf("stackrd = %q, want the new one", b)
	}
}

func TestSwapCommitNeedsPending(t *testing.T) {
	if err := SwapCommit(t.TempDir()); err == nil {
		t.Fatal("commit with nothing pending must fail")
	}
}
