package job

import (
	"slices"
	"testing"

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
		{"same tile same kind", store.Job{Kind: "deploy", LockSet: []string{"a"}}, store.Job{Kind: "deploy", LockSet: []string{"a"}}, true, true},
		{"covers the older", store.Job{Kind: "deploy", LockSet: []string{"a", "b"}}, store.Job{Kind: "deploy", LockSet: []string{"a"}}, true, true},
		{"covers only part", store.Job{Kind: "deploy", LockSet: []string{"a"}}, store.Job{Kind: "deploy", LockSet: []string{"a", "b"}}, true, false},
		{"other kind waits", store.Job{Kind: "deploy", LockSet: []string{"a"}}, store.Job{Kind: "backup", LockSet: []string{"a"}}, true, false},
		{"disjoint", store.Job{Kind: "deploy", LockSet: []string{"a"}}, store.Job{Kind: "deploy", LockSet: []string{"b"}}, false, false},
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
