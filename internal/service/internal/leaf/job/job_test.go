package job

import (
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestPredicates(t *testing.T) {
	all := []string{Queued, Running, Waiting, Done, Failed, Superseded, Cancelled}
	live := []string{Queued, Running, Waiting}
	for _, s := range all {
		if Live(s) != slices.Contains(live, s) || Terminal(s) == Live(s) {
			t.Errorf("%s: live=%v terminal=%v", s, Live(s), Terminal(s))
		}
		if Cancellable(s) != (s == Queued || s == Waiting) {
			t.Errorf("%s: cancellable=%v", s, Cancellable(s))
		}
	}
}

func TestLockRules(t *testing.T) {
	if got := LockSet("b", "a", "b"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("LockSet = %v", got)
	}
	tests := []struct {
		name          string
		newer, older  store.Job
		overlap, supd bool
	}{
		{
			"same tile same kind",
			store.Job{Kind: "deploy", LockSet: []string{"a"}},
			store.Job{Kind: "deploy", LockSet: []string{"a"}},
			true,
			true,
		},
		{
			"covers the older",
			store.Job{Kind: "deploy", LockSet: []string{"a", "b"}},
			store.Job{Kind: "deploy", LockSet: []string{"a"}},
			true,
			true,
		},
		{
			"covers only part",
			store.Job{Kind: "deploy", LockSet: []string{"a"}},
			store.Job{Kind: "deploy", LockSet: []string{"a", "b"}},
			true,
			false,
		},
		{
			"other kind waits",
			store.Job{Kind: "deploy", LockSet: []string{"a"}},
			store.Job{Kind: "backup", LockSet: []string{"a"}},
			true,
			false,
		},
		{
			"disjoint",
			store.Job{Kind: "deploy", LockSet: []string{"a"}},
			store.Job{Kind: "deploy", LockSet: []string{"b"}},
			false,
			false,
		},
	}
	for _, tt := range tests {
		if got := Overlaps(tt.newer.LockSet, tt.older.LockSet); got != tt.overlap {
			t.Errorf("%s: Overlaps = %v", tt.name, got)
		}
		if got := Supersedes(tt.newer, tt.older); got != tt.supd {
			t.Errorf("%s: Supersedes = %v", tt.name, got)
		}
	}
}

// All overlaps every lock set, the empty one included; plain keys still
// overlap only on a shared tile.
func TestOverlapsAll(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{"all vs key", []string{All}, []string{"t1"}, true},
		{"key vs all", []string{"t1", "t2"}, []string{All}, true},
		{"all vs empty", []string{All}, nil, true},
		{"empty vs all", nil, []string{All}, true},
		{"all vs all", []string{All}, []string{All}, true},
		{"shared key", []string{"t1"}, []string{"t1", "t2"}, true},
		{"disjoint", []string{"t1"}, []string{"t2"}, false},
		{"empty vs key", nil, []string{"t1"}, false},
	}
	for _, tt := range tests {
		if got := Overlaps(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: Overlaps = %v, want %v", tt.name, got, tt.want)
		}
	}
	// A newer upgrade replaces a queued one; a deploy never replaces it.
	up := store.Job{Kind: "upgrade", LockSet: []string{All}}
	if !Supersedes(up, up) || Supersedes(store.Job{Kind: "deploy", LockSet: []string{"t1"}}, up) {
		t.Error("supersede rule over All")
	}
}

// Waits: the All job counts the running jobs ahead of it; a job queued
// after an All job, or while the queue is held, waits for the upgrade.
func TestWaits(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 30, 0, m, 0, 0, time.UTC) }
	run1 := store.Job{ID: "r1", State: Running, LockSet: []string{"t1"}, CreatedAt: at(0)}
	run2 := store.Job{ID: "r2", State: Running, LockSet: nil, CreatedAt: at(1)}
	older := store.Job{ID: "q0", State: Queued, LockSet: []string{"t3"}, CreatedAt: at(2)}
	up := store.Job{ID: "up", State: Queued, LockSet: []string{All}, CreatedAt: at(3)}
	after := store.Job{ID: "q1", State: Queued, LockSet: []string{"t9"}, CreatedAt: at(4)}
	parked := store.Job{ID: "w", State: Waiting, LockSet: []string{"t1"}, CreatedAt: at(0)}
	live := []store.Job{run1, run2, older, up, after, parked}

	if got := Ahead(up, live); len(got) != 3 {
		t.Errorf("Ahead(up) = %v, want both running and the older queued", got)
	}
	if got := RunningAhead(up, live); got != 2 {
		t.Errorf("RunningAhead(up) = %d, want 2", got)
	}
	tests := []struct {
		name string
		j    store.Job
		live []store.Job
		held bool
		want string
	}{
		{"upgrade behind two", up, live, false, "Waits for 2 running jobs"},
		{"upgrade behind one", up, []store.Job{run1, up}, false, "Waits for 1 running job"},
		{"upgrade free", up, []store.Job{older, up}, false, ""},
		{"queued after the upgrade", after, live, false, "Waits for the panel upgrade"},
		{"queued before the upgrade", older, live, false, ""},
		{"queue held", older, nil, true, "Waits for the panel upgrade"},
		{"not queued", run1, live, true, ""},
		{"plain tile wait", store.Job{ID: "q2", State: Queued, LockSet: []string{"t1"}, CreatedAt: at(0)}, []store.Job{run1}, false, ""},
	}
	for _, tt := range tests {
		if got := Waits(tt.j, tt.live, tt.held); got != tt.want {
			t.Errorf("%s: Waits = %q, want %q", tt.name, got, tt.want)
		}
	}
}
