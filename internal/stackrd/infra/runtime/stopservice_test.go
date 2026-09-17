package runtime

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
)

// StopService waits on this count, and it is the whole safety of a volume move
// and a backup: rsync or rm -rf must not start while anything could still be
// writing. A task that has no container yet counts, it is about to get one.
func TestLiveTasksCountsEverythingNotYetShutDown(t *testing.T) {
	task := func(s swarm.TaskState) swarm.Task {
		return swarm.Task{Status: swarm.TaskStatus{State: s}}
	}
	live := []swarm.TaskState{
		swarm.TaskStateRunning, swarm.TaskStateStarting, swarm.TaskStatePreparing,
		swarm.TaskStateAssigned, swarm.TaskStateAccepted,
	}
	for _, st := range live {
		assert.Equal(t, 1, liveTasks([]swarm.Task{task(st)}), "%s must count as live", st)
	}
	gone := []swarm.TaskState{
		swarm.TaskStateShutdown, swarm.TaskStateComplete, swarm.TaskStateFailed,
		swarm.TaskStateRejected, swarm.TaskStateOrphaned, swarm.TaskStateRemove,
	}
	for _, st := range gone {
		assert.Equal(t, 0, liveTasks([]swarm.Task{task(st)}), "%s is not holding anything open", st)
	}
	assert.Equal(t, 0, liveTasks(nil), "no tasks is the state we are waiting for")
	assert.Equal(t, 2, liveTasks([]swarm.Task{
		task(swarm.TaskStateRunning), task(swarm.TaskStateShutdown), task(swarm.TaskStateAssigned),
	}))
}

// A service already on a network but without the aliases the caller needs is
// not attached the way the caller needs. A shared database attached by the
// deploy engine with no aliases, then attached again by the provisioner with
// its tile alias, used to keep the alias-less entry, so the name never
// resolved and every s3 slice failed with "connection refused".
func TestMergeAliasesOnlyGrowsWhenItHasTo(t *testing.T) {
	got, grew := mergeAliases(nil, []string{"tile-abc"})
	assert.True(t, grew, "an alias-less attachment has to gain the alias")
	assert.Equal(t, []string{"tile-abc"}, got)

	got, grew = mergeAliases([]string{"tile-abc"}, []string{"tile-abc"})
	assert.False(t, grew, "an unchanged set must not roll the service")
	assert.Equal(t, []string{"tile-abc"}, got)

	got, grew = mergeAliases([]string{"keep-me"}, []string{"tile-abc"})
	assert.True(t, grew)
	assert.Equal(t, []string{"keep-me", "tile-abc"}, got, "existing aliases are never dropped")

	_, grew = mergeAliases([]string{"keep-me"}, nil)
	assert.False(t, grew, "asking for no aliases changes nothing")

	_, grew = mergeAliases(nil, []string{""})
	assert.False(t, grew, "an empty alias is not an alias")
}

// Converged plus a baseline is what tells "the update I just asked for has
// finished" from "a previous update finished and this one has not started".
// They read identically, and the second one made a caller exec into the
// container that was about to be killed: "the database system is shutting
// down" in the middle of cutting a slice.
func TestConvergedNeedsTheUpdateItWasAskedAbout(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	now := time.Now()

	settled := ServiceState{Wanted: 1, Running: 1, Update: "completed", UpdateStarted: old}
	assert.True(t, settled.Converged(), "one replica up and no update in flight")
	assert.True(t, !settled.UpdateStarted.Before(old), "its own baseline is satisfied")
	assert.False(t, !settled.UpdateStarted.Before(now),
		"a roll asked for now is not the one that finished an hour ago")

	rolled := ServiceState{Wanted: 1, Running: 1, Update: "completed", UpdateStarted: now.Add(time.Second)}
	assert.True(t, rolled.Converged() && !rolled.UpdateStarted.Before(now),
		"an update that began after the baseline and settled is the one asked for")

	// The plain cases Converged already had to get right.
	assert.False(t, ServiceState{Wanted: 1, Running: 0, Update: "completed"}.Converged(),
		"nothing running is not converged")
	assert.False(t, ServiceState{Wanted: 0, Running: 0}.Converged(),
		"a service scaled to zero is never converged; StopService is the tool for that")
	assert.False(t, ServiceState{Wanted: 1, Running: 1, Update: "updating"}.Converged(),
		"an update in flight is not settled")
	assert.True(t, ServiceState{Wanted: 1, Running: 1}.Converged(),
		"a service that has never been updated is settled")
}

// A service brought up from zero replicas has no update for swarm to record,
// so the only sign the change landed is that every running task is newer than
// it. An older task still running is the previous spec and does not count.
func TestRolledSinceFromZeroReplicas(t *testing.T) {
	since := time.Now()
	fresh := ServiceState{Wanted: 1, Running: 1, OldestRunning: since.Add(time.Second)}
	assert.True(t, fresh.RolledSince(since), "a task created after the change is the change")

	stale := ServiceState{Wanted: 1, Running: 1, OldestRunning: since.Add(-time.Minute)}
	assert.False(t, stale.RolledSince(since), "a task from before the change is the old spec")

	assert.False(t, ServiceState{Wanted: 1, Running: 0}.RolledSince(since), "nothing running")
}
