package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// SP1: the cadence used to be parsed in the handler, and a value that did not
// parse flashed and left the setting alone — which reads on the page as a save
// that worked. The refusal is the service's now, and it is a refusal.
func TestTheImageCheckIntervalIsValidatedNotCoerced(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	w := &service.ImageWatchService{Store: store}

	assert.Equal(t, 5, w.Interval(ctx), "nothing configured reads as the default, not as off")

	for _, bad := range []string{"", "soon", "-1", "5.5"} {
		_, ok := svcerr.IsInvalid(w.SetInterval(ctx, bad))
		assert.True(t, ok, "%q should be refused", bad)
	}

	require.NoError(t, w.SetInterval(ctx, " 15 "))
	assert.Equal(t, 15, w.Interval(ctx))

	require.NoError(t, w.SetInterval(ctx, "0"))
	assert.Equal(t, 0, w.Interval(ctx), "zero is the watch off, not the default")
}
