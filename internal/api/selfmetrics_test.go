package api_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GET /metrics is stackr's own Prometheus text, for a server admin only.
func TestSelfMetrics(t *testing.T) {
	w := newWorld(t)
	get := func(key string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Accept-Encoding", "gzip") // as every scraper sends it
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, req)
		var r io.Reader = rec.Body
		if rec.Header().Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(rec.Body)
			if err != nil {
				t.Fatal(err)
			}
			r = zr
		}
		b, _ := io.ReadAll(r)
		return rec.Code, string(b)
	}
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", code)
	}
	if code, _ := get(w.owner); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("org owner = %d", code)
	}
	code, body := get(w.admin)
	if code != http.StatusOK || !strings.Contains(body, "go_goroutines") || strings.Contains(body, "caddy_") {
		t.Fatalf("admin = %d %.200s", code, body)
	}
}
