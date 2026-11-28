package deploy_test

import (
	"errors"
	"io"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/startguard"
)

// The storage guard of the job's context runs before the first container
// change, whatever swap marker the caller passed: nothing is created or
// started and the guard's error comes back.
func TestStartGuardStopsARolloutBeforeAnyContainerChange(t *testing.T) {
	w := setup(t)
	gctx := startguard.With(ctx, func() error { return errors.New("the disk failed meanwhile") })
	_, err := w.f.Run(gctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	if err == nil || err.Error() != "the disk failed meanwhile" {
		t.Fatalf("err = %v, want the guard's", err)
	}
	for _, c := range w.fake.Calls() {
		switch c.Method {
		case "Create", "Run", "Start", "Stop", "StopRemove":
			t.Errorf("a container was changed: %v", c)
		}
	}
}

func TestStartGuardThatPassesChangesNothing(t *testing.T) {
	w := setup(t)
	gctx := startguard.With(ctx, func() error { return nil })
	if _, err := w.f.Run(gctx, w.tile, "nginx@sha256:aa", io.Discard, nil); err != nil {
		t.Fatal(err)
	}
}
