package scheduler

import (
	"context"
	"testing"
)

// The whole point of the service is that the nil guard lives in one place: 28
// call sites used to carry their own, and the ones that forgot are why a nil
// backup service panicked a delete. A nil *Service and nil halves must both be
// no-ops, because tests and the API router run without either.
func TestNilSafe(t *testing.T) {
	ctx := context.Background()
	var none *Service
	none.ReloadCron(ctx)
	none.ReloadBackups(ctx)
	none.Reload(ctx)

	empty := New(nil, nil)
	empty.Reload(ctx)
	if cronErr, bkErr := empty.Boot(ctx); cronErr != nil || bkErr != nil {
		t.Fatalf("Boot with no halves = %v, %v; want nil, nil", cronErr, bkErr)
	}
}
