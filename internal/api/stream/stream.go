// Package stream is the API's long responses: server-sent events for job
// and log follows, a raw pipe for exec. Transport only; the handlers pass
// in the verb.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// Heartbeat keeps idle proxies from closing a quiet stream.
var Heartbeat = 15 * time.Second

// PollEvery is how often a job follow re-reads the job.
var PollEvery = 500 * time.Millisecond

// detach runs the stream past the server's per-request timeout: the hamr
// timeout is a deadline on the request context, a client that leaves
// cancels it. Only the second ends the stream.
// ponytail: a stream lives until its end or a failed write; a server-wide
// cap on open streams if they pile up.
func detach(c echo.Context) (ctx context.Context, gone func() bool) {
	req := c.Request().Context()
	return context.WithoutCancel(req), func() bool { return errors.Is(req.Err(), context.Canceled) }
}

func start(c echo.Context) {
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, "text/event-stream")
	h.Set(echo.HeaderCacheControl, "no-cache")
	h.Set("X-Accel-Buffering", "no")
	c.Response().WriteHeader(http.StatusOK)
	c.Response().Flush()
}

func event(c echo.Context, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	c.Response().Flush()
	return nil
}

func ping(c echo.Context) error {
	if _, err := io.WriteString(c.Response(), ": ping\n\n"); err != nil {
		return err
	}
	c.Response().Flush()
	return nil
}

// Poll follows something read by offset: poll returns what to send, the
// next offset and whether it has ended. An event goes out whenever the
// offset moves, and once more at the end ("end").
func Poll(c echo.Context, poll func(ctx context.Context, offset int64) (v any, next int64, end bool, err error)) error {
	ctx, gone := detach(c)
	v, next, end, err := poll(ctx, 0)
	if err != nil {
		return err // before any byte: the JSON error path still applies
	}
	start(c)
	offset := int64(-1)
	lastWrite := time.Now()
	for {
		switch {
		case end:
			return event(c, "end", v)
		case next != offset:
			if err := event(c, "update", v); err != nil {
				return nil
			}
			offset, lastWrite = next, time.Now()
		case time.Since(lastWrite) > Heartbeat:
			if ping(c) != nil {
				return nil
			}
			lastWrite = time.Now()
		}
		if gone() {
			return nil
		}
		time.Sleep(PollEvery)
		if v, next, end, err = poll(ctx, offset); err != nil {
			return event(c, "error", err.Error())
		}
	}
}

// Lines sends each line as a "line" event until the source closes or the
// client leaves; stop releases the source.
func Lines(c echo.Context, lines <-chan string, stop func()) error {
	defer stop()
	_, gone := detach(c)
	start(c)
	tick := time.NewTicker(Heartbeat)
	defer tick.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return event(c, "end", nil)
			}
			if event(c, "line", l) != nil {
				return nil
			}
		case <-tick.C:
			if ping(c) != nil || gone() {
				return nil
			}
		}
	}
}

// Pipe copies r to the response as it comes, flushing each read.
func Pipe(c echo.Context, contentType string, r io.Reader) error {
	c.Response().Header().Set(echo.HeaderContentType, contentType)
	c.Response().WriteHeader(http.StatusOK)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := c.Response().Write(buf[:n]); werr != nil {
				return nil
			}
			c.Response().Flush()
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nil // the status is sent; the cut stream is the signal
		}
	}
}
