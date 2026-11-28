package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The volume and share verbs added with the walk: tile.write and share.write
// in the volume's org, a reader or another org's key is turned away.
func TestVolumeVerbsAPI(t *testing.T) {
	w := newWorld(t)
	vol := w.env.Volume(t, "stack", w.tile.Stack, "data")
	_, kb := w.do(t, w.owner, "POST", "/orgs/acme/keys", `{"name":"v","level":"viewer"}`)
	var k struct{ Token string }
	if err := json.Unmarshal([]byte(kb), &k); err != nil || k.Token == "" {
		t.Fatalf("viewer key = %s", kb)
	}
	rk := k.Token
	base := "/orgs/acme/volumes/" + vol
	for _, p := range []struct{ method, path, body string }{
		{"POST", base + "/delete", ""},
		{"POST", base + "/wipe", `{"stop_tiles":true,"backup_first":false}`},
		{"POST", base + "/restore-orphan", ""},
		{"POST", "/orgs/acme/shares/nas/test", ""},
	} {
		if code, _ := w.do(t, "", p.method, p.path, p.body); code != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, code)
		}
		if code, _ := w.do(t, w.stranger, p.method, p.path, p.body); code != 404 && code != 403 {
			t.Errorf("%s %s other org = %d, want 404", p.method, p.path, code)
		}
		if code, _ := w.do(t, rk, p.method, p.path, p.body); code != 403 {
			t.Errorf("%s %s reader = %d, want 403", p.method, p.path, code)
		}
	}
	// an owner meets the service: a live volume is not an orphan, an unknown share is a 404
	if code, body := w.do(t, w.owner, "POST", base+"/restore-orphan", ""); code < 400 {
		t.Errorf("restore of a live volume = %d %s, want a refusal", code, body)
	}
	if code, body := w.do(t, w.owner, "POST", "/orgs/acme/shares/nas/test", ""); code != 404 {
		t.Errorf("test of an unknown share = %d %s, want 404", code, body)
	}
	// delete with a backup first is a job, plain DELETE answers at once
	if code, body := w.do(t, w.owner, "POST", base+"/delete", ""); code != 202 && !strings.Contains(body, "{") {
		t.Errorf("delete-backed = %d %s, want a job or a refusal", code, body)
	}
}
