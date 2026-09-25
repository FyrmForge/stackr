package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPushGet(t *testing.T) {
	var mu sync.Mutex
	var live string
	var inFlight, maxInFlight int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /load":
			mu.Lock()
			inFlight++
			maxInFlight = max(maxInFlight, inFlight)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			inFlight--
			if strings.Contains(string(b), "bad") {
				mu.Unlock()
				http.Error(w, `{"error":"loading config: bad"}`, http.StatusBadRequest)
				return
			}
			live = string(b)
			mu.Unlock()
		case "GET /config/":
			mu.Lock()
			_, _ = w.Write([]byte(live))
			mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL + "/")
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := c.Push(ctx, []byte(`{"apps":{}}`)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if maxInFlight != 1 {
		t.Fatalf("pushes overlapped: %d in flight", maxInFlight)
	}
	if got, err := c.Get(ctx); err != nil || string(got) != `{"apps":{}}` {
		t.Fatalf("get: %s %v", got, err)
	}
	if err := c.Push(ctx, []byte(`{"bad":1}`)); err == nil || !strings.Contains(err.Error(), "loading config: bad") {
		t.Fatalf("rejected push: %v", err)
	}
}
