package main

import "testing"

// An access line reads as method, path, status and duration; any other line
// as its message.
func TestLogLineText(t *testing.T) {
	acc := map[string]any{"source": "access", "msg": "GET /a 200", "http": map[string]any{
		"http.method": "GET", "http.path": "/a?x=1", "http.status": "200", "http.duration_ms": "12.5",
	}}
	if got := logLineText(acc); got != "GET /a?x=1 200 12.5ms" {
		t.Errorf("access = %q", got)
	}
	if got := logLineText(map[string]any{"source": "app", "msg": "hello"}); got != "hello" {
		t.Errorf("app = %q", got)
	}
}
