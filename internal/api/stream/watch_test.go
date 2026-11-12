package stream

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// A viewer that goes away cancels the ctx a Watch poll reads with, so a slow
// read for a gone client stops; the request timeout (a deadline) does not.
func TestWatchPollEndsWithViewer(t *testing.T) {
	PollEvery = 5 * time.Millisecond
	ctx, away := context.WithCancel(context.Background())
	c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil).WithContext(ctx), httptest.NewRecorder())
	started, ended := make(chan struct{}), make(chan error, 1)
	go func() {
		_ = Watch(c, func(ctx context.Context) ([]Msg, error) {
			close(started)
			<-ctx.Done() // a read that lasts until the viewer is gone
			ended <- ctx.Err()
			return nil, ctx.Err()
		})
	}()
	<-started
	away()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("poll ctx ended without an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll kept reading for a viewer that left")
	}
}
