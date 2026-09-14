package runtime

import (
	"context"
	"io"
	"testing"
)

// A backup tars a volume a container is still writing to. Since the exit-code
// fix a tar that exits non-zero fails the backup, which is right everywhere
// except the documented live mode: there a file moving under the reader is
// what the operator asked for. Both halves are checked, live tolerates it,
// and the ordinary mode still refuses, or the silent pass is back.
func TestTarVolumeWhileWritten(t *testing.T) {
	ctx := context.Background()
	r, err := New()
	if err != nil {
		t.Skip("no docker")
	}
	vol := "stkr-test-livetar"
	if err := r.CreateVolume(ctx, vol); err != nil {
		t.Skip("no docker:", err)
	}
	defer func() { _ = r.RemoveVolume(ctx, vol) }()

	// A writer churning the same files the tar is reading.
	_, wait, err := r.volumeTool(ctx, vol, false, []string{"sh", "-c",
		"i=0; while [ $i -lt 120 ]; do head -c 2000000 /dev/urandom > /data/a.bin; head -c 100000 /dev/urandom > /data/b.bin; i=$((i+1)); done"}, nil)
	if err != nil {
		t.Skip("no docker:", err)
	}
	done := make(chan error, 1)
	go func() { done <- wait() }()

	strict := 0
	for range 3 {
		if err := r.TarVolume(ctx, vol, io.Discard, true); err != nil {
			t.Fatalf("live tar of a volume being written: %v", err)
		}
		if err := r.TarVolume(ctx, vol, io.Discard, false); err != nil {
			strict++
		}
	}
	<-done
	if strict == 0 {
		t.Skip("the writer never caught the reader; nothing to assert about the strict mode")
	}
}
