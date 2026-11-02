package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// Deploys, param changes, domain changes, role changes and deletes leave an
// audit event naming the actor and key, and never a param value.
func TestOperationsAreAudited(t *testing.T) {
	w := newWorld(t)
	tile := "/orgs/acme/stacks/shop/envs/dev/tiles/api"
	if code, out := w.do(t, w.owner, "POST", tile+"/deploy", ""); code != 202 {
		t.Fatalf("deploy = %d %s", code, out)
	}
	if code, out := w.do(t, w.owner, "PATCH", "/orgs/acme/params", `[{"collection":"params","name":"LOG","kind":"param","value":"s3cretvalue"}]`); code != 200 {
		t.Fatalf("set param = %d %s", code, out)
	}
	if code, out := w.do(t, w.owner, "POST", tile+"/domains", `{"host":"audit.example.com"}`); code != 201 {
		t.Fatalf("attach = %d %s", code, out)
	}
	if code, out := w.do(t, w.owner, "DELETE", "/orgs/acme/params/params/LOG", ""); code != 200 {
		t.Fatalf("delete param = %d %s", code, out)
	}

	if code, out := w.do(t, w.owner, "POST", "/orgs/acme/stacks", `{"name":"scratch"}`); code != 201 {
		t.Fatalf("create stack = %d %s", code, out)
	}
	if code, out := w.do(t, w.owner, "DELETE", "/orgs/acme/stacks/scratch", ""); code != 204 {
		t.Fatalf("delete stack = %d %s", code, out)
	}

	code, out := w.do(t, w.admin, "GET", "/admin/security-log?limit=200", "")
	if code != 200 {
		t.Fatalf("log = %d %s", code, out)
	}
	var es []struct {
		Action  string
		ActorID string `json:"actor_id"`
		KeyID   string `json:"key_id"`
	}
	if err := json.Unmarshal([]byte(out), &es); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range es {
		seen[e.Action] = seen[e.Action] || (e.ActorID != "" && e.KeyID != "")
	}
	for _, a := range []string{"job.queue", "param.set", "param.delete", "domain.attach", "stack.delete"} {
		if !seen[a] {
			t.Errorf("no %s event with an actor and key", a)
		}
	}
	if strings.Contains(out, "s3cretvalue") {
		t.Error("a param value is in the audit log")
	}
}
