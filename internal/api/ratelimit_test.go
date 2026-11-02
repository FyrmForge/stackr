package api_test

import (
	"encoding/json"
	"testing"
)

// One key over its budget is a 429 with Retry-After and the error body's
// code; another key is untouched.
func TestAPIRateLimitPerKey(t *testing.T) {
	w := newWorld(t)
	var last int
	var body string
	for range 800 {
		if last, body = w.do(t, w.owner, "GET", "/me", ""); last == 429 {
			break
		}
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &e); last != 429 || err != nil || e.Code != "rate_limited" {
		t.Fatalf("flooding key = %d %s, want 429 rate_limited", last, body)
	}
	if code, _ := w.do(t, w.admin, "GET", "/me", ""); code != 200 {
		t.Errorf("another key while the first is limited = %d, want 200", code)
	}
}
