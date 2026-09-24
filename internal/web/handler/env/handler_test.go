package env_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The env stream opens with the rendered lanes (none yet: an empty svg the
// canvas swaps in place), as HTML, not JSON.
func TestEnvEventsSendsLanes(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	s := webtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	rec := s.DoCtx(ctx, t, "GET", "/acme/shop/dev/events", nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(body, "event: traffic\ndata: <svg data-edges data-lanes id=\"graph-lanes\"") {
		t.Fatalf("events = %d %q", rec.Code, body)
	}
}
