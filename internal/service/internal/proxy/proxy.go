// Package proxy is the Caddy admin API client: it ships a whole config and
// reads it back. Building the config (routes, upstreams, auth, extras) is
// leaf/domain's; this package only moves JSON.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client talks to one Caddy admin endpoint, e.g. http://stackr-proxy:2019.
type Client struct {
	Addr string
	HTTP *http.Client

	mu sync.Mutex // one push at a time: a reload mid-reload is a race
}

func New(addr string) *Client {
	return &Client{Addr: strings.TrimRight(addr, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Push replaces Caddy's whole config (POST /load). Caddy applies it
// atomically: it returns 200 with the new config live, or an error with the
// old one still serving, so there is nothing to probe afterwards. The
// config must carry the admin listener itself: without it Caddy falls back
// to localhost:2019 inside its container, even on a rejected push, and stackrd
// loses the proxy until it restarts.
// extract: coalescing concurrent saves into exactly one follow-up run belongs
// where the config is built (a follow-up must rebuild from the database, not
// resend a stale body), and so does running a handler's push off the request
// context.
func (c *Client) Push(ctx context.Context, config json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.do(ctx, http.MethodPost, "/load", config)
	return err
}

// Get returns Caddy's live config (GET /config/); "null" when it has none.
func (c *Client) Get(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/config/", nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("caddy %s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}
