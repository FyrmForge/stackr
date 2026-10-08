package main

import (
	"strings"
	"testing"
)

// --level server works the server scope: /admin/params, whatever is linked,
// and a refusal of an unknown level names the new one.
func TestParamsServerLevel(t *testing.T) {
	r := fake(t, map[string]string{
		"/params":         `[{"collection":"s3","name":"region","kind":"param","value":"eu"}]`,
		"/params/secrets": `[{"collection":"s3","name":"secret_key","kind":"secret","value":"sk"}]`,
	})
	if code, _, errw := cli(t, "params", "set", "s3.region=eu", "--level", "server"); code != 0 {
		t.Fatalf("set = %d %s", code, errw)
	}
	if code, out, errw := cli(t, "params", "get", "--level", "server", "--reveal"); code != 0 || !strings.Contains(out, "sk") {
		t.Fatalf("get --reveal = %d %q %q", code, out, errw)
	}
	if code, _, errw := cli(t, "-y", "params", "rm", "s3.region", "--level", "server"); code != 0 {
		t.Fatalf("rm = %d %s", code, errw)
	}
	want := []string{
		`PATCH /api/v1/admin/params [{"collection":"s3","kind":"param","name":"region","value":"eu"}]`,
		"GET /api/v1/admin/params/secrets ",
		"GET /api/v1/admin/params ",
		"DELETE /api/v1/admin/params/s3/region ",
	}
	if len(r.reqs) != len(want) {
		t.Fatalf("requests = %q, want %q", r.reqs, want)
	}
	for i, w := range want {
		if r.reqs[i] != w {
			t.Errorf("request %d = %q, want %q", i, r.reqs[i], w)
		}
	}
	if code, _, errw := cli(t, "params", "get", "--level", "cluster"); code != 2 || !strings.Contains(errw, "org, stack, env or server") {
		t.Errorf("bad level = %d %q", code, errw)
	}
}
