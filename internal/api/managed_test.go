package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Step 7b task 6: an allow edit is an owner's (a member gets 403); env
// pairs, slice tiles, access and on_remove are a member's; a viewer reads.
func TestManagedAndSliceRoutes(t *testing.T) {
	w := newWorld(t)
	u := w.env.User(t, "member@x", false)
	w.env.Member(t, w.acme, u, "member")
	member := w.env.APIKey(t, u, w.acme)
	v := w.env.User(t, "viewer@x", false)
	w.env.Member(t, w.acme, v, "viewer")
	viewer := w.env.APIKey(t, v, w.acme)
	if _, err := w.env.Orch.CreateManagedTile(
		context.Background(),
		service.Tile{EnvironmentID: w.tile.Env, Name: "pg"},
		"postgres",
	); err != nil {
		t.Fatal(err)
	}
	const env = "/orgs/acme/stacks/shop/envs/dev"
	want := func(key, method, path, body string, code int, has ...string) {
		t.Helper()
		got, out := w.do(t, key, method, path, body)
		if got != code {
			t.Fatalf("%s %s = %d %s, want %d", method, path, got, out, code)
		}
		for _, s := range has {
			if !strings.Contains(out, s) {
				t.Errorf("%s %s: no %s in %s", method, path, s, out)
			}
		}
	}

	allow := `{"allow":["acme:shop:*"]}`
	want(member, "PUT", env+"/tiles/pg/allow", allow, 403)
	want(w.owner, "PUT", env+"/tiles/pg/allow", allow, 200, `"allow":["acme:shop:*"]`)
	want(w.owner, "PUT", env+"/tiles/pg/allow", `{"allow":["other:shop:*"]}`, 400)
	want(w.owner, "PUT", env+"/tiles/api/allow", allow, 400)
	want(viewer, "PUT", env+"/tiles/pg/env-pairs", `{"env_pairs":{"dev":"dev"}}`, 403)
	want(member, "PUT", env+"/tiles/pg/env-pairs", `{"env_pairs":{"dev":"dev"}}`, 200, `"env_pairs":{"dev":"dev"}`)
	want(member, "PUT", env+"/tiles/pg/env-pairs", `{"env_pairs":{"dev":"prod"}}`, 400)

	want(viewer, "POST", env+"/slices", `{"name":"db","provision_from":"shop:dev:pg"}`, 403)
	want(member, "POST", env+"/slices", `{"name":"db","provision_from":"shop:dev"}`, 400)
	want(member, "POST", env+"/slices", `{"name":"db","provision_from":"shop:dev:pg"}`, 201,
		`"kind":"slice"`, `"default_access":"write"`, `"on_remove":"keep"`)
	want(viewer, "GET", env+"/tiles/db/slice", "", 200, `"target":"shop:dev:pg"`, `"blocker":""`)
	want(viewer, "GET", env+"/tiles/db/bindings", "", 200, "[]")
	want(viewer, "GET", env+"/tiles/api/slice", "", 400)

	want(member, "PUT", env+"/tiles/api/slice-access", `{"slice":"db","access":"read"}`, 200,
		`"slice_access":[{"from":"db","access":"read"}]`)
	want(member, "PUT", env+"/tiles/api/slice-access", `{"slice":"db","access":"default"}`, 200,
		`"slice_access":[]`)
	want(member, "PUT", env+"/tiles/pg/slice-access", `{"slice":"db","access":"read"}`, 400)
	want(member, "PUT", env+"/tiles/db/on-remove", `{"on_remove":"drop"}`, 200, `"on_remove":"drop"`)
	want(member, "PUT", env+"/tiles/api/on-remove", `{"on_remove":"drop"}`, 400)
}
