package deploy_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
)

// PhaseError keeps its wrapped error's text and identity; PhaseOf finds it
// through further wrapping and says "" for any other error.
func TestPhaseError(t *testing.T) {
	t.Parallel()
	base := errs.Conflictf("nothing running")
	pe := &deploy.PhaseError{Phase: deploy.PhaseStart, Err: base}
	if pe.Error() != base.Error() {
		t.Errorf("text = %q, want %q", pe.Error(), base.Error())
	}
	wrapped := fmt.Errorf("deploy: %w", pe)
	if got := deploy.PhaseOf(wrapped); got != "start" {
		t.Errorf("PhaseOf = %q", got)
	}
	if _, ok := errs.IsConflict(wrapped); !ok {
		t.Error("the typed error is lost under the phase")
	}
	if got := deploy.PhaseOf(errors.New("x")); got != "" {
		t.Errorf("PhaseOf(plain) = %q", got)
	}
	if got := deploy.PhaseOf(nil); got != "" {
		t.Errorf("PhaseOf(nil) = %q", got)
	}
}

// Every error out of the rollout is tagged "start", whichever step of it
// failed; an error before the rollout (the pull) has no phase.
func TestRunTagsTheRolloutAsStart(t *testing.T) {
	w := mountWorld(t)
	w.fake.Err = map[string]error{"Create": errors.New("range of CPUs is from 0.01 to 1.00")}
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	if err == nil || deploy.PhaseOf(err) != deploy.PhaseStart {
		t.Fatalf("create error: %v, phase %q", err, deploy.PhaseOf(err))
	}
	if err.Error() != "range of CPUs is from 0.01 to 1.00" {
		t.Errorf("text changed: %q", err)
	}

	w = mountWorld(t)
	w.fake.Err = map[string]error{"Start(new)": errors.New("port is already allocated")}
	if _, err = w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil); deploy.PhaseOf(err) != deploy.PhaseStart {
		t.Errorf("start error phase %q", deploy.PhaseOf(err))
	}

	w = setup(t)
	w.fake.Err = map[string]error{"Pull": errors.New("registry down")}
	if _, err = w.f.Run(ctx, w.tile, "nginx@sha256:bb", io.Discard, nil); err == nil || deploy.PhaseOf(err) != "" {
		t.Errorf("pull error: %v, phase %q", err, deploy.PhaseOf(err))
	}
}

func TestExtPlanError(t *testing.T) {
	t.Parallel()
	if err := deploy.ExtPlanError("postgres", []string{"vchord"}, []string{"vector"}); err != nil {
		t.Errorf("postgres: %v", err)
	}
	if err := deploy.ExtPlanError("s3", nil, nil); err != nil {
		t.Errorf("empty lists on s3: %v", err)
	}
	for _, c := range [][2][]string{{{"vchord"}, nil}, {nil, {"vector"}}} {
		err := deploy.ExtPlanError("s3", c[0], c[1])
		if err == nil || err.Error() != "s3 has no extensions; only postgres does" {
			t.Errorf("s3 %v: %v", c, err)
		}
	}
}
