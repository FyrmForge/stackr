package s3

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	_ Destination = Local{}
	_ Destination = Bucket{}
)

// roundTrip is the contract every destination meets; the minio test reuses it.
func roundTrip(t *testing.T, d Destination) {
	t.Helper()
	ctx := context.Background()
	for _, k := range []string{"org/b/2-x", "org/a/1-x", "org/b/1-x", "other/1-x"} {
		if err := d.Put(ctx, k, strings.NewReader("data "+k)); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	if err := d.Put(ctx, "org/a/1-x", strings.NewReader("replaced")); err != nil {
		t.Fatal(err)
	}
	keys, err := d.List(ctx, "org/")
	if err != nil || !slices.Equal(keys, []string{"org/a/1-x", "org/b/1-x", "org/b/2-x"}) {
		t.Fatalf("list: %v %v", keys, err)
	}
	rc, err := d.Get(ctx, "org/a/1-x")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "replaced" {
		t.Fatalf("get: %q", got)
	}
	if _, err := d.Get(ctx, "org/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if err := d.Delete(ctx, "org/b/1-x"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "org/b/1-x"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if err := Probe(ctx, d); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if keys, _ := d.List(ctx, ""); !slices.Equal(keys, []string{"org/a/1-x", "org/b/2-x", "other/1-x"}) {
		t.Fatalf("list after delete: %v", keys)
	}
}

func TestLocal(t *testing.T) {
	root := t.TempDir()
	d := Local{Dir: filepath.Join(root, "dest")}
	if keys, err := d.List(context.Background(), ""); err != nil || keys != nil {
		t.Fatalf("list before first put: %v %v", keys, err)
	}
	roundTrip(t, d)

	// "../" cannot leave Dir: it lands inside it.
	if err := d.Put(context.Background(), "../../escape", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "escape")); err != nil {
		t.Fatalf("confined key not inside Dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); err == nil {
		t.Fatal("key escaped Dir")
	}
	for _, bad := range []string{"", "/", "..", "a/.tmp-x"} {
		if err := d.Put(context.Background(), bad, strings.NewReader("x")); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}
