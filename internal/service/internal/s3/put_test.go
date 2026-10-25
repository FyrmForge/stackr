package s3

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repeat is n bytes of a pattern, read without holding them.
type repeat struct{ n int }

func (r *repeat) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.n)
	for i := range p[:n] {
		p[i] = byte(r.n - i)
	}
	r.n -= n
	return n, nil
}

type failReader struct{ err error }

func (r *failReader) Read([]byte) (int, error) { return 0, r.err }

// An unsized stream larger than one part goes up as a multipart upload, whole.
func TestBucketPutStreamsMultipart(t *testing.T) {
	f := newFakeS3(t)
	const size = 40<<20 + 123
	if err := f.bucket().Put(context.Background(), "k", &repeat{size}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Objects["/b/k"]); got != size {
		t.Errorf("stored %d bytes, want %d", got, size)
	}
	if len(f.Open) != 0 {
		t.Errorf("%d multipart uploads left open", len(f.Open))
	}
}

// A stream that dies partway aborts the upload: no object, no stray parts.
func TestBucketPutErrorAborts(t *testing.T) {
	f := newFakeS3(t)
	r := io.MultiReader(&repeat{40 << 20}, &failReader{errors.New("source died")})
	if err := f.bucket().Put(context.Background(), "k", r); err == nil {
		t.Fatal("a broken stream uploaded")
	}
	if len(f.Objects) != 0 || len(f.Open) != 0 || f.Aborted != 1 {
		t.Errorf("objects %d, open %d, aborted %d", len(f.Objects), len(f.Open), f.Aborted)
	}
}

// A refused part aborts too.
func TestBucketPutRefusedPartAborts(t *testing.T) {
	f := newFakeS3(t)
	f.FailPart = 2
	if err := f.bucket().Put(context.Background(), "k", &repeat{40 << 20}); err == nil {
		t.Fatal("a refused part uploaded")
	}
	if len(f.Objects) != 0 || len(f.Open) != 0 {
		t.Errorf("objects %d, open %d", len(f.Objects), len(f.Open))
	}
}

// Local renames into place only when the whole body arrived.
func TestLocalPutIsAtomic(t *testing.T) {
	dir := t.TempDir()
	l := Local{Dir: dir}
	ctx := context.Background()
	cut := io.MultiReader(strings.NewReader("half"), &failReader{errors.New("cut")})
	if err := l.Put(ctx, "a/1", cut); err == nil {
		t.Fatal("a cut body stored")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "a", "*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if err := l.Put(ctx, "a/1", strings.NewReader("whole")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a", "1")); string(b) != "whole" {
		t.Errorf("stored %q", b)
	}
}
