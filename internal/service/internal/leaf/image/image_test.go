package image_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func TestCleanupKeepsReleased(t *testing.T) {
	st := servicetest.Store(t)
	fake := dockerfake.New()
	l := image.New(st.Images, fake)

	released, _ := l.Built(ctx, "stkr/a:1111111", "sha256:r")
	old, _ := l.Built(ctx, "stkr/a:2222222", "sha256:o")
	fresh, _ := l.Built(ctx, "stkr/a:3333333", "sha256:f")
	watched, _ := l.Checked(ctx, "nginx:1", "sha256:w", "", nil)
	// Age the unreleased build past the grace period.
	aged := time.Now().Add(-2 * image.Grace)
	old.BuiltAt = &aged
	if err := st.Images.Update(ctx, old); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Cleanup(ctx, []string{released.ID}); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	keep := calls[len(calls)-1].Args
	for _, want := range []string{released.Ref, released.Digest, fresh.Ref} {
		if !slices.Contains(keep, want) {
			t.Errorf("keep %v misses %s", keep, want)
		}
	}
	if slices.Contains(keep, old.Ref) {
		t.Errorf("keep %v holds the unreleased old build", keep)
	}
	for _, id := range []string{released.ID, fresh.ID, watched.ID} {
		if _, err := l.Get(ctx, id); err != nil {
			t.Errorf("row %s gone: %v", id, err)
		}
	}
	if _, err := l.Get(ctx, old.ID); err == nil {
		t.Error("old build's row kept")
	}
}

func TestNameAndWatchCache(t *testing.T) {
	st := servicetest.Store(t)
	l := image.New(st.Images, dockerfake.New())
	ref, _ := l.Name(ctx, "stkr/a", "abcdef123", "job-12345678")
	if ref != "stkr/a:abcdef1" {
		t.Fatal(ref)
	}
	_, _ = l.Built(ctx, ref, "sha256:x")
	if again, _ := l.Name(ctx, "stkr/a", "abcdef123", "job-12345678"); again != "stkr/a:abcdef1-job-1234" {
		t.Errorf("second build of a commit = %s", again)
	}

	_, _ = l.Checked(ctx, "nginx:1", "sha256:a", "", nil)
	i, _ := l.Checked(ctx, "nginx:1", "", "", errors.New("429"))
	if i.LastDigest != "sha256:a" || i.LastError != "429" {
		t.Errorf("an error dropped the last good answer: %+v", i)
	}
	if i, _ = l.Checked(ctx, "nginx:1", "sha256:b", "", nil); i.LastError != "" || i.LastDigest != "sha256:b" {
		t.Errorf("success kept the error: %+v", i)
	}
}
