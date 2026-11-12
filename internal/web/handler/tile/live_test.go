package tile_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/web/handler/tile"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The drawer opens one live stream, carrying the header's regions and, on the
// Status tab, that tab's; each is sent once at connect and again only when its
// markup changes.
func TestTileLiveStream(t *testing.T) {
	stream.PollEvery, tile.LiveEvery = 10*time.Millisecond, 0
	s := webtest.New(t)
	page := s.Do(t, "GET", drawer+"?tab=status", nil).Body.String()
	if n := strings.Count(page, "sse-connect="); n != 1 {
		t.Errorf("the drawer holds %d streams, want 1", n)
	}
	for _, want := range []string{
		`sse-connect="` + drawer + `/live?tab=status"`,
		`sse-swap="actions"`, `sse-swap="status"`, `sse-swap="lastjob"`, `sse-swap="urls"`, `sse-swap="access"`,
		`hx-trigger="sse:word"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("status tab lacks %q", want)
		}
	}
	for tab, regions := range map[string][]string{
		"jobs":   {"actions"},
		"status": {"actions", "status", "lastjob", "urls", "access"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(300*time.Millisecond, cancel)
		body := s.DoCtx(ctx, t, "GET", drawer+"/live?tab="+tab, nil).Body.String()
		for _, region := range regions {
			if n := strings.Count(body, "event: "+region+"\n"); n != 1 {
				t.Errorf("%s: %s sent %d times, want once (it did not change):\n%s", tab, region, n, body)
			}
		}
		if strings.Contains(body, "event: word") {
			t.Errorf("%s: the status word did not change, yet word was sent", tab)
		}
	}
}

// The Jobs tab polls itself, the Runs tab and the empty Logs tab too.
func TestTilePolls(t *testing.T) {
	s := webtest.New(t)
	for tab, want := range map[string]string{
		"jobs": `hx-get="` + drawer + `?tab=jobs"`,
		"logs": `hx-trigger="every 3s"`,
	} {
		if body := s.Do(t, "GET", drawer+"?tab="+tab, nil).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s tab does not poll (%q):\n%s", tab, want, body)
		}
	}
	for tab, want := range map[string]string{
		"settings": `id="live-domains"`,
	} {
		if body := s.Do(t, "GET", drawer+"?tab="+tab, nil).Body.String(); !strings.Contains(body, want) || !strings.Contains(body, `hx-select="#live-domains"`) {
			t.Errorf("%s tab lacks the self-polling domains list", tab)
		}
	}
}
