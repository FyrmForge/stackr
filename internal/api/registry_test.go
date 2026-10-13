package api_test

import "testing"

// The registry routes: an org's repos belong to its members alone, another
// org's repo is a 404 on both verbs, a delete names its version, and the
// overview and the cleanup are the server admin's.
func TestRegistryRoutes(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		key, method, path string
		want              int
	}{
		{w.stranger, "GET", "/orgs/acme/registry", 404},
		{w.stranger, "GET", "/orgs/acme/registry/versions?repo=acme/shop/api", 404},
		{w.stranger, "DELETE", "/orgs/acme/registry/versions?repo=acme/shop/api&tag=v1", 404},
		{w.owner, "GET", "/orgs/acme/registry/versions?repo=other/shop/api", 404},
		{w.owner, "DELETE", "/orgs/acme/registry/versions?repo=other/shop/api&tag=v1", 404},
		{w.owner, "DELETE", "/orgs/acme/registry/versions?repo=acme/shop/api", 400},
		{w.owner, "GET", "/admin/registry", 403},
		{w.owner, "POST", "/admin/registry/clean", 403},
		{"", "GET", "/orgs/acme/registry", 401},
	} {
		if code, body := w.do(t, c.key, c.method, c.path, ""); code != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.path, code, c.want, body)
		}
	}
}
