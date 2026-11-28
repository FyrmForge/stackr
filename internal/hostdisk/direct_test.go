package hostdisk

import (
	"fmt"
	"syscall"
	"testing"
)

func TestIOFailure(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{syscall.EIO, true},
		{fmt.Errorf("read: %w", syscall.EUCLEAN), true},
		{&Error{CodeIO, "read the stamp: " + syscall.EIO.Error()}, true},
		{&Error{CodeIO, syscall.ENOSPC.Error()}, false},
		{&Error{CodeIO, syscall.EDQUOT.Error()}, false},
		{syscall.ENOSPC, false},
		{syscall.EDQUOT, false},
		{&Error{CodeDiskMissing, syscall.EIO.Error()}, false},
	} {
		if got := IOFailure(c.err); got != c.want {
			t.Errorf("IOFailure(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
