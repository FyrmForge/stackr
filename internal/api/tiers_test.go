package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tiers: an owner adds, renames, orders, locks and deletes; a member reads
// but writes none of it. An env lock is a member's and not a viewer's.
func TestTierRoutes(t *testing.T) {
	w := newWorld(t)
	mu, vu := w.env.User(t, "mem@x", false), w.env.User(t, "view@x", false)
	w.env.Member(t, w.acme, mu, "member")
	w.env.Member(t, w.acme, vu, "viewer")
	member, viewer := w.env.APIKey(t, mu, w.acme), w.env.APIKey(t, vu, w.acme)

	call := func(key, m, p, body string, want int) string {
		t.Helper()
		code, out := w.do(t, key, m, "/orgs/acme"+p, body)
		if code != want {
			t.Fatalf("%s %s = %d %s, want %d", m, p, code, out, want)
		}
		return out
	}
	for _, s := range []string{"dev", "prod"} {
		call(w.owner, "POST", "/tiers", `{"slug":"`+s+`"}`, 201)
	}
	call(member, "POST", "/tiers", `{"slug":"qa"}`, 403)
	call(member, "PUT", "/tiers/dev", `{"slug":"qa"}`, 403)
	call(member, "PUT", "/tiers/order", `{"slugs":["prod","dev"]}`, 403)
	call(member, "PUT", "/tiers/dev/lock", `{"locked":false}`, 403)
	call(member, "DELETE", "/tiers/dev", "", 403)

	call(w.owner, "PUT", "/tiers/dev", `{"slug":"stage"}`, 409) // shop/dev is in it
	call(w.owner, "PUT", "/stacks/shop/envs/dev/name", `{"name":"local"}`, 200)
	call(w.owner, "PUT", "/tiers/dev", `{"slug":"stage"}`, 200)
	call(w.owner, "PUT", "/stacks/shop/envs/local/name", `{"name":"dev"}`, 200)
	call(w.owner, "PUT", "/tiers/order", `{"slugs":["prod","stage"]}`, 204)
	call(w.owner, "PUT", "/tiers/prod/lock", `{"locked":false}`, 200)
	out := call(viewer, "GET", "/tiers", "", 200)
	var ts []struct {
		Slug   string
		Locked bool
	}
	if json.Unmarshal([]byte(out), &ts) != nil || len(ts) != 2 || ts[0].Slug != "prod" || ts[0].Locked {
		t.Errorf("tiers = %s, want prod (unlocked) then stage", out)
	}
	call(w.owner, "PUT", "/tiers/nope/lock", `{"locked":true}`, 404)

	// tier params: the member writes, the viewer reads names only
	call(member, "PATCH", "/tiers/stage/params", `[{"collection":"db","name":"host","kind":"param","value":"h"}]`, 200)
	call(w.owner, "PATCH", "/tiers/stage/params", `[{"collection":"db","name":"pw","kind":"secret","value":"LEAK"}]`, 200)
	if out := call(viewer, "GET", "/tiers/stage/params", "", 200); strings.Contains(out, "LEAK") || !strings.Contains(out, `"pw"`) {
		t.Errorf("masked list = %s", out)
	}
	call(viewer, "GET", "/tiers/stage/params/secrets", "", 403)
	call(viewer, "PATCH", "/tiers/stage/params", `[]`, 403)
	call(w.owner, "GET", "/tiers/nope/params", "", 404)
	call(w.owner, "DELETE", "/tiers/stage/params/db/host", "", 200)
	call(w.owner, "PATCH", "/pr/params", `[{"collection":"db","name":"host","kind":"param","value":"sandbox"}]`, 200)
	call(w.owner, "PATCH", "/stacks/shop/pr/params", `[{"collection":"app","name":"mode","kind":"param","value":"pr"}]`, 200)
	if out := call(viewer, "GET", "/stacks/shop/pr/params", "", 200); !strings.Contains(out, `"mode"`) || strings.Contains(out, "sandbox") {
		t.Errorf("stack pr list = %s", out)
	}
	call(viewer, "GET", "/pr/params/secrets", "", 403)
	call(w.owner, "DELETE", "/pr/params/db/host", "", 200)

	// env lock: dev is off-tier here (the tier is stage), so the lock moves
	const dev = "/stacks/shop/envs/dev/lock"
	call(viewer, "PUT", dev, `{"locked":false}`, 403)
	call(member, "PUT", dev, `{"locked":false}`, 200)
	call(w.owner, "PUT", "/tiers/stage", `{"slug":"dev"}`, 200)
	call(member, "PUT", dev, `{"locked":false}`, 409) // now dev is a tier's env

	call(w.owner, "DELETE", "/tiers/prod", "", 204)
	call(w.owner, "DELETE", "/tiers/dev", "", 409) // shop/dev is in it
}
