package jobs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// A schedule tick on a stack mid config-apply skips; a manual run does not.
// Release undoes exactly one Hold.
func TestHoldSkipsScheduleTicks(t *testing.T) {
	s := NewService(nil, nil, nil)
	app := &repo.Tile{StackID: "st"}
	ctx := context.Background()

	assert.Equal(t, "", s.skipReason(ctx, app, TriggerSchedule))
	s.Hold("st")
	s.Hold("st")
	assert.Equal(t, "config apply in progress", s.skipReason(ctx, app, TriggerSchedule))
	assert.Equal(t, "", s.skipReason(ctx, app, TriggerManualWeb), "a manual run must not be held")
	s.Release("st")
	assert.Equal(t, "config apply in progress", s.skipReason(ctx, app, TriggerSchedule), "one release undid two holds")
	s.Release("st")
	assert.Equal(t, "", s.skipReason(ctx, app, TriggerSchedule))
}

func TestDepBlocked(t *testing.T) {
	assert.Equal(t, "api is not running", depBlocked("api", &repo.Tile{Status: "stopped"}))
	assert.Equal(t, "api is not running", depBlocked("api:healthy", &repo.Tile{Status: "waiting:DB_URL"}))
	assert.Equal(t, "", depBlocked("api", &repo.Tile{Status: "running"}))
	assert.Equal(t, "seed has not completed", depBlocked("seed:completed", &repo.Tile{LastStatus: "error"}))
	assert.Equal(t, "", depBlocked("seed:completed", &repo.Tile{LastStatus: "ok"}))
	assert.Equal(t, "", depBlocked("ghost", nil), "a missing dependency is the deploy's problem")
}
