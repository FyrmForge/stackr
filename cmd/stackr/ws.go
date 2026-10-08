package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// dial opens a websocket to an API path with the same server and key as
// request. No client timeout: a shell or a tunnel lives as long as it is
// used. A refused upgrade is the server's error, so it reads like any
// other command's. The response carries the upgrade headers.
func (a *app) dial(path string, q url.Values) (*websocket.Conn, *http.Response, error) {
	key := a.env("KEY", a.cfg.Key)
	if a.server() == "" || key == "" {
		return nil, nil, errors.New("not logged in; run stackr login <url>")
	}
	u := strings.TrimRight(a.server(), "/") + "/api/v1" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	ws, res, err := websocket.Dial(a.ctx, u, &websocket.DialOptions{HTTPHeader: h, HTTPClient: &http.Client{}})
	if err == nil {
		return ws, res, nil
	}
	if res == nil || res.StatusCode < 300 {
		return nil, nil, fmt.Errorf("can't reach %s: %w", a.server(), err)
	}
	e := &apiErr{Status: res.StatusCode}
	if b, _ := io.ReadAll(io.LimitReader(res.Body, 4096)); len(b) > 0 {
		_ = json.Unmarshal(b, e)
	}
	if e.Msg == "" {
		e.Msg = http.StatusText(res.StatusCode)
	}
	return nil, res, e
}
