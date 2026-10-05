package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCLIPlumbing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tiles/x") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"mem_limit_mb: must not be negative"}`))
	}))
	useServer(t, srv.URL)
	if code, _, errw := cli(t, "tile", "get", "x", "--stack", "shop", "--env", "dev"); code != 1 || !strings.Contains(errw, `no tile "x" in shop/dev`) {
		t.Fatalf("404: %d %q", code, errw)
	}
	if _, _, errw := cli(t, "tile", "set", "y", "--stack", "shop", "--env", "dev", "--port", "1"); !strings.Contains(errw, "--memory: must") {
		t.Fatalf("field: %q", errw)
	}
	srv.Close()
	if code, _, errw := cli(t, "tile", "ls", "--stack", "shop", "--env", "dev"); code != 1 || !strings.Contains(errw, "can't reach "+srv.URL) {
		t.Fatalf("dial: %d %q", code, errw)
	}
	if code, _, errw := cli(t, "tile", "lss"); code != 2 || !strings.Contains(errw, "did you mean") {
		t.Fatalf("suggest: %d %q", code, errw)
	}
	if code, _, _ := cli(t, "help", "nope"); code != 2 {
		t.Fatalf("help: %d", code)
	}
	if code, _, errw := cli(t, "stack", "ls", "--stack", "x"); code != 2 || !strings.Contains(errw, "unknown flag") {
		t.Fatalf("scope: %d %q", code, errw)
	}
	if code, _, _ := cli(t, "tile", "ls", "--stack", "x", "--env", "y", "--help"); code != 0 {
		t.Fatal("scoped help")
	}
}
