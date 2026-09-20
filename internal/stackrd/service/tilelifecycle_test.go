package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// lifeStore answers the two calls the cron paths make. Everything else panics
// on the nil embedded interface, which is what keeps this test honest: a new
// store call inside these methods fails here rather than passing quietly.
type lifeStore struct {
	repo.Store
	wrote string
	run   *repo.CronRun
}

func (s *lifeStore) UpdateTileStatus(_ context.Context, _, status string) error {
	s.wrote = status
	return nil
}

func (s *lifeStore) GetCronRun(context.Context, string) (*repo.CronRun, error) {
	return s.run, nil
}

// newLife builds the service with no cluster, no job runner and no notifier.
// The cron paths must not need any of them; if one starts to, this panics.
func newLife(st repo.Store) *TileLifecycleService {
	return NewTileLifecycleService(st, nil, nil, nil, nil, nil)
}

func cronTile(status string) *repo.Tile {
	return &repo.Tile{ID: "t1", StackID: "s1", Kind: "cron", Status: status}
}

// The point-3 pick: a cron parks as "paused", not "stopped". "stopped" is the
// state the reconciler skips for run-to-completion kinds, so a cron parked
// that way is parked for ever and still ticks.
func TestStopParksACronAsPaused(t *testing.T) {
	st := &lifeStore{}
	tile := cronTile("idle")
	if err := newLife(st).Stop(context.Background(), tile); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if st.wrote != "paused" {
		t.Errorf("wrote %q, want paused", st.wrote)
	}
	if tile.Status != "paused" {
		t.Errorf("the caller's tile still says %q", tile.Status)
	}
}

func TestStopRefusesAVolume(t *testing.T) {
	err := newLife(&lifeStore{}).Stop(context.Background(), &repo.Tile{ID: "v1", Kind: "volume"})
	if _, ok := svcerr.IsInvalid(err); !ok {
		t.Fatalf("want an invalid, got %v (%T)", err, err)
	}
}

func TestToggleCron(t *testing.T) {
	st := &lifeStore{}
	got, err := newLife(st).ToggleCron(context.Background(), cronTile("paused"))
	if err != nil || got != "idle" {
		t.Fatalf("paused cron should resume: %q %v", got, err)
	}
	// The guard the panel did not have: a service has no schedule, and
	// parking one at "paused" left the reconciler fighting the row.
	if _, err := newLife(st).ToggleCron(context.Background(),
		&repo.Tile{ID: "t2", Kind: "service"}); err == nil {
		t.Error("a service tile has no schedule to toggle")
	}
}

func TestRunNow(t *testing.T) {
	life := newLife(&lifeStore{})
	if _, err := life.RunNow(context.Background(), &repo.Tile{ID: "t2", Kind: "service"}, Actor{}); err == nil {
		t.Error("run applies to cron and function tiles only")
	}
	// The panel used to nil-deref here rather than answer.
	_, err := life.RunNow(context.Background(), cronTile("idle"), Actor{})
	if !errors.Is(err, svcerr.ErrUnavailable) {
		t.Errorf("no job runner must be unavailable, got %v", err)
	}
}

// A run id belonging to another tile reads as missing, not as someone
// else's row.
func TestStopRunChecksOwnership(t *testing.T) {
	st := &lifeStore{run: &repo.CronRun{ID: "r1", Ref: "app:other"}}
	life := NewTileLifecycleService(st, nil, nil, nil, nil, nil)
	// jobs is nil, so reaching the Stop call would report unavailable; the
	// ownership check has to come first.
	_, err := life.StopRun(context.Background(), cronTile("idle"), "r1")
	if !errors.Is(err, svcerr.ErrNotFound) {
		t.Errorf("want not found, got %v", err)
	}
}

func TestActorString(t *testing.T) {
	for _, c := range []struct {
		in   Actor
		want string
	}{
		{Actor{Name: "Ada", Email: "a@b.com", Via: "web"}, "a@b.com"},
		{Actor{Name: "ci-key", Via: "api"}, "ci-key (api)"},
		{Actor{Via: "api"}, "(api)"},
		{Actor{}, ""},
	} {
		if got := c.in.String(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.in, got, c.want)
		}
	}
}
