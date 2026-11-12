package api_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

const execPath = "/orgs/acme/stacks/shop/envs/dev/tiles/api/exec"

// A command that outlives ?timeout= is stopped: the output says so and the
// X-Exit-Code trailer is 124, not a hung request.
func TestExecTimeout(t *testing.T) {
	w := newWorld(t)
	w.env.Docker.Containers = []service.Container{{
		ID: "c-1", State: "running",
		Labels: map[string]string{"stackr.tile": w.tile.ID, "stackr.role": "replica"},
	}}
	w.env.Docker.ExecHang = true
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1"+execPath+"?container=c-1&cmd=psql&timeout=200ms", nil)
	req.Header.Set("Authorization", "Bearer "+w.owner)
	w.h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "the command ran past 0.2 s and was stopped") {
		t.Errorf("exec = %d %q, want the stopped message", rec.Code, rec.Body.String())
	}
	if got := rec.Result().Trailer.Get("X-Exit-Code"); got != "124" {
		t.Errorf("exit trailer = %q, want 124", got)
	}
	if code, _ := w.do(t, w.owner, "POST", execPath+"?container=c-1&cmd=x&timeout=2h", ""); code != 422 && code != 400 {
		t.Errorf("timeout 2h = %d, want a validation error", code)
	}
}
