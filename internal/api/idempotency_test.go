package api_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

func (w *world) doKeyed(t *testing.T, key, idem, path string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1"+path, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", idem)
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b), rec.Header().Get("Idempotent-Replay")
}

// A retry with the same Idempotency-Key gets the first job back, not a second
// one; another key, another URL or another caller is a new request. The job
// records who asked.
func TestIdempotencyKey(t *testing.T) {
	w := newWorld(t)
	deploy := "/orgs/acme/stacks/shop/envs/dev/tiles/api/deploy"

	code, first, replay := w.doKeyed(t, w.owner, "k1", deploy)
	var j service.Job
	if err := json.Unmarshal([]byte(first), &j); err != nil || code != 202 || j.ID == "" || replay != "" {
		t.Fatalf("first = %d %s replay=%q", code, first, replay)
	}
	if j.ActorID == "" {
		t.Error("the job does not say who asked for it")
	}
	if code, again, replay := w.doKeyed(t, w.owner, "k1", deploy); code != 202 || again != first || replay != "true" {
		t.Errorf("retry = %d %s replay=%q, want the first answer", code, again, replay)
	}
	var k2 service.Job
	_, second, _ := w.doKeyed(t, w.owner, "k2", deploy)
	if err := json.Unmarshal([]byte(second), &k2); err != nil || k2.ID == j.ID {
		t.Errorf("a new key reused the job: %s", second)
	}
	// the same key from the admin is the admin's own request
	if code, body, replay := w.doKeyed(t, w.admin, "k1", deploy); code != 202 || replay != "" {
		t.Errorf("another principal with the same key = %d %s replay=%q", code, body, replay)
	}
	// a failed request keeps no key: the retry runs
	if code, _, _ := w.doKeyed(t, w.owner, "k3", "/orgs/acme/stacks/shop/envs/dev/tiles/nope/deploy"); code != 404 {
		t.Fatalf("unknown tile = %d", code)
	}
}

// The job list shows the payload as the object it holds.
func TestJobPayloadIsAnObject(t *testing.T) {
	w := newWorld(t)
	_, body := w.do(t, w.owner, "POST", "/orgs/acme/stacks/shop/envs/dev/tiles/api/deploy", "")
	var j struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(body), &j); err != nil || j.Payload["org_id"] == nil {
		t.Errorf("payload = %s (%v)", body, err)
	}
}
