package deploystate

import "testing"

func TestLiveSet(t *testing.T) {
	for _, s := range []string{Queued, Running, WaitingCI} {
		if !IsLive(s) || IsTerminal(s) {
			t.Fatalf("%q should be live", s)
		}
	}
	for _, s := range []string{Done, Error, Cancelled} {
		if IsLive(s) || !IsTerminal(s) {
			t.Fatalf("%q should be terminal", s)
		}
	}
	if !IsCancellable(WaitingCI) || !IsCancellable(Queued) || IsCancellable(Running) {
		t.Fatal("cancellable set is queued+waiting_ci only")
	}
}
