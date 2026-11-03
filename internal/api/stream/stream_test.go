package stream

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// Stop ends an open stream at its next poll, so shutdown never waits on it.
func TestStopEndsStreams(t *testing.T) {
	PollEvery = 10 * time.Millisecond
	t.Cleanup(func() { stopping, stopOnce = make(chan struct{}), sync.Once{} })
	e := echo.New()
	done := make(chan error, 2)
	go func() {
		c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		done <- Watch(c, func(context.Context) ([]Msg, error) { return nil, nil })
	}()
	go func() {
		c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		done <- Lines(c, make(chan string), func() {})
	}()
	time.Sleep(50 * time.Millisecond)
	Stop()
	for range 2 {
		select {
		case err := <-done:
			_ = err // ended by Stop: that it ended is the test
		case <-time.After(2 * time.Second):
			t.Fatal("a stream outlived Stop")
		}
	}
}

// A client back with Last-Event-ID resumes the poll at that offset.
func TestPollResumesFromLastEventID(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Last-Event-ID", "7")
	c := echo.New().NewContext(r, httptest.NewRecorder())
	got := int64(-1)
	_ = Poll(c, func(_ context.Context, off int64) (any, int64, bool, error) {
		got = off
		return "x", 9, true, nil
	})
	if got != 7 {
		t.Fatalf("polled from %d, want 7", got)
	}
}
