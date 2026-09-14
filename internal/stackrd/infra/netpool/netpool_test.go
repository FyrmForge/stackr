package netpool

import "testing"

// The allocator must fill gaps and must never hand out a name a row holds,
// two environments on one overlay is the isolation hole this pool exists to
// close. Growing past the pre-created size is a normal outcome, not an error.
func TestFirstFree(t *testing.T) {
	used := map[string]bool{}
	for i := 1; i <= EnvPoolSize; i++ {
		used[name(EnvPrefix, i)] = true
	}
	if got := firstFree(used, EnvPrefix); got != "stkr-net-33" {
		t.Fatalf("full pool grows: %s", got)
	}
	delete(used, "stkr-net-07")
	if got := firstFree(used, EnvPrefix); got != "stkr-net-07" {
		t.Fatalf("returned name is reused: %s", got)
	}
	// The two pools are separate namespaces; a busy env pool says nothing
	// about the db pool.
	if got := firstFree(used, DBPrefix); got != "stkr-dbnet-01" {
		t.Fatalf("db pool: %s", got)
	}
}
