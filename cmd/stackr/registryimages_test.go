package main

import (
	"strings"
	"testing"
)

// registry ls reads the repos, or one repo's versions.
func TestRegistryLs(t *testing.T) {
	r := fake(t, map[string]string{
		"/registry":          `[{"name":"acme/shop/api","tags":2,"latest":"v2","size":10}]`,
		"/registry/versions": `[{"tag":"v2","platforms":["amd64"],"size":10,"source":"built","used_by":["shop/dev"]}]`,
	})
	if code, out, errw := cli(t, "registry", "ls"); code != 0 || !strings.Contains(out, "acme/shop/api") {
		t.Fatalf("ls = %d %q %q", code, out, errw)
	}
	code, out, errw := cli(t, "registry", "ls", "acme/shop/api")
	if code != 0 || !strings.Contains(out, "shop/dev") || !strings.Contains(strings.Join(r.reqs, "\n"), "GET /api/v1/orgs/acme/registry/versions?repo=acme%2Fshop%2Fapi") {
		t.Fatalf("ls repo = %d %q %q, sent %q", code, out, errw, r.reqs)
	}
}

// registry rm names the version, asks first, refuses a version an env runs
// unless --force (and then names the envs), and deletes by repo and tag.
func TestRegistryRm(t *testing.T) {
	const del = "DELETE /api/v1/orgs/acme/registry/versions?repo=acme%2Fshop%2Fapi&tag="
	answers := map[string]string{
		"/registry/versions": `[{"tag":"v1","used_by":["shop/dev"]},{"tag":"v2","used_by":[]}]`,
	}
	r := fake(t, answers)
	if code, _, errw := cli(t, "registry", "rm", "acme/shop/api:v2"); code == 0 || !strings.Contains(errw, "Delete acme/shop/api:v2?") || len(writes(r)) != 0 {
		t.Errorf("rm without a yes = %d %q, wrote %q", code, errw, writes(r))
	}
	r = fake(t, answers)
	if code, _, errw := cli(t, "registry", "rm", "acme/shop/api:v2", "-y"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != del+"v2 " {
		t.Errorf("rm -y = %d %q, wrote %q", code, errw, writes(r))
	}
	r = fake(t, answers)
	if code, _, errw := cli(t, "registry", "rm", "acme/shop/api:v1", "-y"); code != 2 || !strings.Contains(errw, "shop/dev") || len(writes(r)) != 0 {
		t.Errorf("rm used = %d %q, wrote %q", code, errw, writes(r))
	}
	r = fake(t, answers)
	if code, _, errw := cli(t, "registry", "rm", "acme/shop/api:v1", "--force", "-y"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != del+"v1&force=true " {
		t.Errorf("rm --force = %d %q, wrote %q", code, errw, writes(r))
	}
	if code, _, _ := cli(t, "registry", "rm", "acme/shop/api", "-y"); code != 2 {
		t.Errorf("rm without a tag = %d, want 2", code)
	}
}

// admin registry clean asks, then queues the sweep.
func TestAdminRegistryClean(t *testing.T) {
	r := fake(t, map[string]string{"": `{"id":"j1","state":"queued"}`})
	if code, _, errw := cli(t, "admin", "registry", "clean", "--no-wait"); code == 0 || !strings.Contains(errw, "Clean up the registry now?") || len(writes(r)) != 0 {
		t.Errorf("clean without a yes = %d %q, wrote %q", code, errw, writes(r))
	}
	r = fake(t, map[string]string{"": `{"id":"j1","state":"queued"}`})
	if code, _, errw := cli(t, "admin", "registry", "clean", "--no-wait", "-y"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != "POST /api/v1/admin/registry/clean " {
		t.Errorf("clean -y = %d %q, wrote %q", code, errw, writes(r))
	}
}
