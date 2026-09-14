package runtime

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// Exercises every volume op the panel offers, after the exit-code fix turned
// silent-pass into real failure.
func TestVolumeOpsRoundTrip(t *testing.T) {
	ctx := context.Background()
	r, err := New()
	if err != nil {
		t.Skip("no docker")
	}
	vol := "stkr-test-volops"
	if err := r.CreateVolume(ctx, vol); err != nil {
		t.Skip("no docker:", err)
	}
	defer func() { _ = r.RemoveVolume(ctx, vol) }()

	if err := r.WriteVolumeFile(ctx, vol, "/sub/a.txt", strings.NewReader("hello")); err != nil {
		t.Fatal("write:", err)
	}
	es, err := r.ListVolumeFiles(ctx, vol, "/")
	if err != nil {
		t.Fatal("list:", err)
	}
	t.Logf("list / = %+v", es)
	if es2, err := r.ListVolumeFiles(ctx, vol, "/sub"); err != nil || len(es2) != 1 {
		t.Fatalf("list /sub = %v, %v", es2, err)
	}
	// An empty directory is not an error.
	if err := r.WriteVolumeFile(ctx, vol, "/empty/.keep", strings.NewReader("")); err != nil {
		t.Fatal("write .keep:", err)
	}
	if err := r.DeleteVolumeFile(ctx, vol, "/empty/.keep"); err != nil {
		t.Fatal("delete .keep:", err)
	}
	if es3, err := r.ListVolumeFiles(ctx, vol, "/empty"); err != nil || len(es3) != 0 {
		t.Fatalf("list of an empty dir = %v, %v", es3, err)
	}
	// A path that is not there is an error.
	if _, err := r.ListVolumeFiles(ctx, vol, "/nope"); err == nil {
		t.Error("listing a missing dir reported success")
	}

	rc, wait, err := r.ReadVolumeFile(ctx, vol, "/sub/a.txt")
	if err != nil {
		t.Fatal("read:", err)
	}
	b, _ := io.ReadAll(rc)
	if err := wait(); err != nil {
		t.Fatal("read wait:", err)
	}
	if string(b) != "hello" {
		t.Fatalf("read = %q", b)
	}
	if _, w2, err := r.ReadVolumeFile(ctx, vol, "/nope.txt"); err == nil {
		if err := w2(); err == nil {
			t.Error("reading a missing file reported success")
		}
	}

	var tar bytes.Buffer
	if err := r.TarVolume(ctx, vol, &tar, false); err != nil {
		t.Fatal("tar:", err)
	}
	if tar.Len() == 0 {
		t.Fatal("tar produced nothing")
	}
	if err := r.DeleteVolumeFile(ctx, vol, "/sub"); err != nil {
		t.Fatal("delete dir:", err)
	}
	if err := r.UntarVolume(ctx, vol, bytes.NewReader(tar.Bytes())); err != nil {
		t.Fatal("untar:", err)
	}
	rc2, wait2, err := r.ReadVolumeFile(ctx, vol, "/sub/a.txt")
	if err != nil {
		t.Fatal("read after restore:", err)
	}
	b2, _ := io.ReadAll(rc2)
	if err := wait2(); err != nil {
		t.Fatal("read after restore wait:", err)
	}
	if string(b2) != "hello" {
		t.Fatalf("restored file = %q", b2)
	}
}
