package jobs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stop is what turns an error into a "stopped" row: it cancels the run's
// context (which kills the one-shot container) and marks the id, so the
// finish path knows the failure was asked for. A run that is already over
// must answer false rather than silently doing nothing.
func TestStopCancelsAndMarks(t *testing.T) {
	s := NewService(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	assert.False(t, s.Stop("nobody"), "stopped a run that was never held")

	s.hold("run1", cancel)
	require.True(t, s.Stop("run1"), "Stop found no run to cancel")
	assert.Error(t, ctx.Err(), "the run's context was left alive")
	assert.True(t, s.wasStopped("run1"), "the run was not marked stopped, so it would record as an error")

	s.release("run1")
	assert.False(t, s.Stop("run1"), "a finished run is still stoppable")
	assert.False(t, s.wasStopped("run1"), "the stopped mark outlived the run")
}
