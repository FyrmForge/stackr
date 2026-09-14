package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// loginTimeout bounds how long the browser flow waits for approval.
const loginTimeout = 3 * time.Minute

// LoginWithKey saves an explicitly-provided key after verifying it works.
func LoginWithKey(ctx context.Context, rawURL, key string) error {
	cfg := Config{URL: normalizeURL(rawURL), Key: strings.TrimSpace(key)}
	if err := verify(ctx, cfg); err != nil {
		return err
	}
	return Save(cfg)
}

// LoginBrowser runs the gh-style loopback flow: spin up a localhost listener,
// open the browser at /cli/authorize, receive a one-time code on the callback,
// exchange it for a key, verify, and save.
func LoginBrowser(ctx context.Context, rawURL string) error {
	base := normalizeURL(rawURL)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start loopback listener: %w", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	state, err := randHex()
	if err != nil {
		return err
	}

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			done <- result{err: fmt.Errorf("state mismatch; possible spoofed callback")}
			return
		}
		_, _ = io.WriteString(w, "stackr CLI authorized. You can close this tab.")
		done <- result{code: q.Get("code")}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	host, _ := os.Hostname()
	authURL := base + "/cli/authorize?" + url.Values{
		"port":     {strconv.Itoa(port)},
		"state":    {state},
		"hostname": {host},
	}.Encode()

	fmt.Println("Opening your browser to authorize. If it doesn't open, visit:")
	fmt.Println("  " + authURL)
	_ = openBrowser(authURL)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(loginTimeout):
		return fmt.Errorf("timed out waiting for browser approval")
	case res := <-done:
		if res.err != nil {
			return res.err
		}
		key, err := exchange(ctx, base, res.code)
		if err != nil {
			return err
		}
		cfg := Config{URL: base, Key: key}
		if err := verify(ctx, cfg); err != nil {
			return err
		}
		return Save(cfg)
	}
}

// exchange trades the one-time code for the raw key at POST /cli/exchange.
func exchange(ctx context.Context, base, code string) (string, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/cli/exchange", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("exchange failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", fmt.Errorf("exchange returned empty key")
	}
	return out.Key, nil
}

// verify confirms a credential authenticates against the server.
func verify(ctx context.Context, cfg Config) error {
	if _, err := NewClient(cfg).Stacks(ctx); err != nil {
		return fmt.Errorf("key rejected by %s: %w", cfg.URL, err)
	}
	return nil
}

func normalizeURL(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return u
}

func randHex() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openBrowser best-effort opens a URL in the platform browser.
func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
