// Package workerclient is how the controller talks to a worker node: the
// Docker client settings for the node's proxied Docker API, and a typed
// client for the extras API v1 (internal/worker).
package workerclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/FyrmForge/stackr/internal/worker"
)

// Client talks to one node at addr ("host:port") over mTLS.
type Client struct {
	addr string
	hc   *http.Client
}

// New takes the controller's client tls.Config (W4 builds it). HTTP/1.1 only,
// matching the worker: attach and exec hijack the connection.
func New(addr string, tc *tls.Config) *Client {
	tc = tc.Clone()
	tc.NextProtos = []string{"http/1.1"}
	return &Client{addr: addr, hc: &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tc},
	}}
}

// DockerOptions are what the docker package needs to dial the node's Docker
// API: client.WithHost(Host), client.WithHTTPClient(HTTPClient). The
// transport carries the TLS config, so the SDK picks https itself. No
// Timeout: log and event streams are long.
type DockerOptions struct {
	Host       string // tcp://addr
	HTTPClient *http.Client
}

func (c *Client) DockerOptions() DockerOptions {
	tr := c.hc.Transport.(*http.Transport).Clone()
	return DockerOptions{Host: "tcp://" + c.addr, HTTPClient: &http.Client{Transport: tr}}
}

func (c *Client) do(ctx context.Context, method, path, ctype string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return c.hc.Do(req)
}

// call runs a request and decodes a JSON reply into out (nil = ignore body).
func (c *Client) call(ctx context.Context, method, path, ctype string, body []byte, out any) error {
	resp, err := c.do(ctx, method, path, ctype, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("worker %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) Facts(ctx context.Context) (worker.Facts, error) {
	var f worker.Facts
	err := c.call(ctx, "GET", "/v1/facts", "", nil, &f)
	return f, err
}

// Traffic is the node's conntrack lines that touch its own tiles.
func (c *Client) Traffic(ctx context.Context) (worker.Traffic, error) {
	var t worker.Traffic
	err := c.call(ctx, "GET", "/v1/traffic", "", nil, &t)
	return t, err
}

// ApplyIPTables sends an iptables-restore payload (applied with --noflush).
func (c *Client) ApplyIPTables(ctx context.Context, payload string) error {
	return c.call(ctx, "POST", "/v1/iptables", "text/plain", []byte(payload), nil)
}

// ApplyWG sends a wg config in whatever shape wgapply on the node reads.
func (c *Client) ApplyWG(ctx context.Context, config []byte) error {
	return c.call(ctx, "POST", "/v1/wg", "application/json", config, nil)
}

func (c *Client) PutState(ctx context.Context, st worker.State) error {
	b, _ := json.Marshal(st)
	return c.call(ctx, "PUT", "/v1/state", "application/json", b, nil)
}

func (c *Client) GetState(ctx context.Context) (worker.State, error) {
	var st worker.State
	err := c.call(ctx, "GET", "/v1/state", "", nil, &st)
	return st, err
}

func (c *Client) Replay(ctx context.Context) error {
	return c.call(ctx, "POST", "/v1/replay", "", nil, nil)
}

// Caddy passes a request to the node's Caddy admin API. The caller closes
// the response body.
func (c *Client) Caddy(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return c.do(ctx, method, "/v1/caddy"+path, "application/json", body)
}
