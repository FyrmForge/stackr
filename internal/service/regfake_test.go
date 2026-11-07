package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRegistry answers the catalog and tag lists for repos (name to tags) and
// points the orchestrator's registry leaf at it.
func (w *world) fakeRegistry(t *testing.T, repos map[string][]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" {
			names := []string{}
			for n := range repos {
				names = append(names, n)
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"repositories": names})
			return
		}
		if repo, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list"); ok {
			_ = json.NewEncoder(rw).Encode(map[string]any{"name": repo, "tags": repos[repo]})
			return
		}
		http.NotFound(rw, r)
	}))
	t.Cleanup(srv.Close)
	w.orch.regs = w.orch.regs.WithBase(srv.URL)
}
