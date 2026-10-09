package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
)

// Key scope: a viewer-ceiling key reads but cannot deploy; a stack key works
// on its stack and 404s another; a ceiling above the minter is refused.
func TestKeyScope(t *testing.T) {
	w := newWorld(t)
	if code, out := w.do(t, w.owner, "POST", "/orgs/acme/stacks", `{"name":"web"}`); code != 201 {
		t.Fatalf("create stack = %d %s", code, out)
	}
	mint := func(key, body string) (int, string) {
		code, out := w.do(t, key, "POST", "/orgs/acme/keys", body)
		var k struct{ Token string }
		_ = json.Unmarshal([]byte(out), &k)
		if code == 201 {
			return code, k.Token
		}
		return code, out
	}
	_, viewer := mint(w.owner, `{"name":"v","level":"viewer"}`)
	_, shop := mint(w.owner, `{"name":"s","stack":"shop"}`)
	if code, out := mint(w.owner, `{"name":"x","stack":"nope"}`); code != 400 {
		t.Errorf("stack outside the org = %d %s, want 400", code, out)
	}
	if code, _ := mint(w.owner, `{"name":"x","level":"admin"}`); code != 403 {
		t.Errorf("admin ceiling from an owner = %d, want 403", code)
	}
	if code, _ := w.do(t, viewer, "POST", "/orgs/acme/cli-codes", `{"name":"x"}`); code != 403 {
		t.Errorf("capped key asking for a CLI code = %d, want 403", code)
	}
	if code, _ := mint(viewer, `{"name":"x","level":"member"}`); code != 403 {
		t.Errorf("key minting above its own ceiling = %d, want 403", code)
	}

	deploy := "/orgs/acme/stacks/shop/envs/dev/tiles/api/deploy"
	if code, _ := w.do(t, viewer, "GET", "/orgs/acme/stacks/shop", ""); code != 200 {
		t.Errorf("viewer key read = %d, want 200", code)
	}
	if code, _ := w.do(t, viewer, "POST", deploy, ""); code != 403 {
		t.Errorf("viewer key deploy = %d, want 403", code)
	}
	if code, _ := w.do(t, shop, "GET", "/orgs/acme/stacks/shop", ""); code != 200 {
		t.Errorf("stack key on its stack = %d, want 200", code)
	}
	if code, _ := w.do(t, shop, "GET", "/orgs/acme/stacks/web", ""); code != 404 {
		t.Errorf("stack key on another stack = %d, want 404", code)
	}
	if code, _ := w.do(t, shop, "GET", "/orgs/acme/stacks", ""); code != 403 {
		t.Errorf("stack key on an org-wide route = %d, want 403", code)
	}
	code, out := w.do(t, w.owner, "GET", "/me/keys", "")
	var ks []struct{ Level, Stack string }
	if err := json.Unmarshal([]byte(out), &ks); code != 200 || err != nil {
		t.Fatalf("keys = %d %s", code, out)
	}
	var sawLevel, sawStack bool
	for _, k := range ks {
		sawLevel = sawLevel || k.Level == "viewer"
		sawStack = sawStack || k.Stack == "shop"
	}
	if !sawLevel || !sawStack {
		t.Errorf("key list lacks level or stack: %s", out)
	}
}

// A stack key follows its own deploy and meets another stack's ids as 404,
// whatever route parent they ride in; it revokes only itself.
func TestStackKeyRows(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	w := newWorld(t)
	other := w.env.Stack(t, w.acme, "web")
	_, out := w.do(t, w.owner, "POST", "/orgs/acme/keys", `{"name":"s","stack":"shop"}`)
	var k struct{ Token string }
	if err := json.Unmarshal([]byte(out), &k); err != nil || k.Token == "" {
		t.Fatalf("mint = %s", out)
	}
	shop := k.Token

	_, body := w.do(t, shop, "POST", "/orgs/acme/stacks/shop/envs/dev/tiles/api/deploy", "")
	var j service.Job
	if err := json.Unmarshal([]byte(body), &j); err != nil || j.ID == "" {
		t.Fatalf("deploy = %s", body)
	}
	if code, out := w.do(t, shop, "GET", "/orgs/acme/jobs/"+j.ID+"/events", ""); code != 200 {
		t.Errorf("stack key on its own job events = %d %s, want 200", code, out)
	}
	_, body = w.do(t, w.owner, "POST", "/orgs/acme/stacks/web/envs/dev/tiles/api/deploy", "")
	var oj service.Job
	if err := json.Unmarshal([]byte(body), &oj); err != nil || oj.ID == "" {
		t.Fatalf("deploy web = %s", body)
	}
	if code, _ := w.do(t, shop, "GET", "/orgs/acme/jobs/"+oj.ID+"/events", ""); code != 404 {
		t.Errorf("stack key on another stack's job = %d, want 404", code)
	}

	dom := w.env.Domain(t, other.ID, "web.example.com")
	rel := w.env.Release(t, other.Stack, 1)
	vol := w.env.Volume(t, "stack", other.Stack, "data")
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/orgs/acme/stacks/shop/envs/dev/tiles/api/domains/" + dom, `{}`},
		{"DELETE", "/orgs/acme/stacks/shop/envs/dev/tiles/api/domains/" + dom, ""},
		{"GET", "/orgs/acme/stacks/shop/releases/" + rel, ""},
		{"POST", "/orgs/acme/volumes/" + vol + "/backups", ""},
	} {
		if code, out := w.do(t, shop, c.method, c.path, c.body); code != 404 {
			t.Errorf("stack key %s %s = %d %s, want 404", c.method, c.path, code, out)
		}
	}
	// the right id under the wrong route parent is a 404 for anyone
	if code, _ := w.do(t, w.owner, "GET", "/orgs/acme/stacks/shop/releases/"+rel, ""); code != 404 {
		t.Errorf("another stack's release under this stack = %d, want 404", code)
	}

	// a stack key revokes only itself
	w.do(t, w.owner, "POST", "/orgs/acme/keys", `{"name":"o"}`)
	_, list := w.do(t, w.owner, "GET", "/me/keys", "")
	var ks []struct{ ID, Name string }
	_ = json.Unmarshal([]byte(list), &ks)
	var plain, own string
	for _, x := range ks {
		switch x.Name {
		case "o":
			plain = x.ID
		case "s":
			own = x.ID
		}
	}
	if plain == "" || own == "" {
		t.Fatalf("keys = %s", list)
	}
	if code, _ := w.do(t, shop, "DELETE", "/me/keys/"+plain, ""); code != 403 {
		t.Errorf("stack key revoking another key = %d, want 403", code)
	}
	if code, _ := w.do(t, shop, "DELETE", "/me/keys/"+own, ""); code != 204 && code != 200 {
		t.Errorf("stack key revoking itself = %d, want 2xx", code)
	}
	if code, _ := w.do(t, w.owner, "DELETE", "/me/keys/"+plain, ""); code != 204 && code != 200 {
		t.Errorf("uncapped key revoking another = %d, want 2xx", code)
	}
}

// A stack key follows its image-check, restore and tile delete jobs (the
// payload carries stack_id, so a gone tile still places the job), restores
// only inside its own stack, and gets 404 for a missing job as for a foreign one.
func TestStackKeyJobsAndRestore(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	w := newWorld(t)
	other := w.env.Stack(t, w.acme, "web")
	_, out := w.do(t, w.owner, "POST", "/orgs/acme/keys", `{"name":"s","stack":"shop"}`)
	var k struct{ Token string }
	if err := json.Unmarshal([]byte(out), &k); err != nil || k.Token == "" {
		t.Fatalf("mint = %s", out)
	}
	shop := k.Token
	job := func(method, path, body string) string {
		t.Helper()
		code, out := w.do(t, shop, method, path, body)
		var j service.Job
		if err := json.Unmarshal([]byte(out), &j); err != nil || j.ID == "" || code >= 300 {
			t.Fatalf("%s %s = %d %s", method, path, code, out)
		}
		return j.ID
	}
	events := func(id string, want int) {
		t.Helper()
		if code, out := w.do(t, shop, "GET", "/orgs/acme/jobs/"+id+"/events", ""); code != want {
			t.Errorf("stack key on job %s events = %d %s, want %d", id, code, out, want)
		}
	}

	events("missing", 404)
	events(job("POST", "/orgs/acme/stacks/shop/image-check", ""), 200)

	own := w.env.Volume(t, "stack", w.tile.Stack, "data")
	own2 := w.env.Volume(t, "stack", w.tile.Stack, "data2")
	foreign := w.env.Volume(t, "stack", other.Stack, "data")
	events(job("POST", "/orgs/acme/volumes/"+own+"/restore", `{"run_id":"r","target_volume_id":"`+own2+`"}`), 200)
	for _, c := range [][2]string{{own, foreign}, {foreign, own}} {
		if code, out := w.do(t, shop, "POST", "/orgs/acme/volumes/"+c[0]+"/restore",
			`{"run_id":"r","target_volume_id":"`+c[1]+`"}`); code != 404 {
			t.Errorf("restore %s into %s = %d %s, want 404", c[0], c[1], code, out)
		}
	}
	// the org owner may copy across stacks
	if code, _ := w.do(t, w.owner, "POST", "/orgs/acme/volumes/"+own+"/restore",
		`{"run_id":"r","target_volume_id":"`+foreign+`"}`); code != 202 {
		t.Errorf("owner cross-stack restore = %d, want 202", code)
	}

	id := job("DELETE", "/orgs/acme/stacks/shop/envs/dev/tiles/api", "")
	eventually(t, "tile row gone", func() bool {
		_, err := w.env.Store.Tiles.Get(context.Background(), w.tile.ID)
		return err != nil
	})
	events(id, 200)
}
