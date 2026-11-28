package worker

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
)

// An I/O error from the read is a fresh answer (IOErr, not stale); ENOSPC and
// a quota error are neither. A read-only mount shows in ReadOnly.
func TestDisksFactsReportIOErrorsAndReadOnly(t *testing.T) {
	for _, c := range []struct {
		err       error
		ioErr     bool
		wantStale bool
	}{{syscall.EIO, true, false}, {syscall.ENOSPC, false, true}, {syscall.EDQUOT, false, true}} {
		s := diskServer(t, rootLine+strings.Replace(dataLine, "/mnt/data rw", "/mnt/data ro", 1), func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
			return hostdisk.Stamp{}, 0, 0, c.err
		})
		if err := s.PutDisks([]DiskReg{regA}); err != nil {
			t.Fatal(err)
		}
		if c.ioErr {
			eventually(t, "io error read", func() bool { return facts(s).Disks[0].IOErr })
		} else {
			time.Sleep(50 * time.Millisecond)
		}
		f := facts(s)
		if f.Disks[0].IOErr != c.ioErr || f.DisksStale != c.wantStale || !f.Disks[0].ReadOnly {
			t.Errorf("%v: io=%v stale=%v ro=%v", c.err, f.Disks[0].IOErr, f.DisksStale, f.Disks[0].ReadOnly)
		}
	}
}
