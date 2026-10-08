package main

import (
	"strings"
	"testing"
)

// The server connector verbs send what the API reads; sharing with every
// org, or with none, asks first.
func TestServerConnectorRoutes(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	const c = "/api/v1/admin/connectors"
	for _, tc := range []struct{ args, want string }{
		{"ls", "GET " + c + " "},
		{"rename c1 main", "PUT " + c + `/c1/name {"name":"main"}`},
		{"share c1 --org o1 --org o2", "PUT " + c + `/c1/shares {"all":false,"org_ids":["o1","o2"]}`},
		{"share c1 --all -y", "PUT " + c + `/c1/shares {"all":true,"org_ids":[]}`},
		{"share c1 -y", "PUT " + c + `/c1/shares {"all":false,"org_ids":[]}`},
		{"rm c1 -y", "DELETE " + c + "/c1 "},
	} {
		r.reqs = nil
		if code, _, errw := cli(t, append([]string{"server", "connectors"}, strings.Fields(tc.args)...)...); code != 0 {
			t.Errorf("server connectors %s = %d %s", tc.args, code, errw)
			continue
		}
		if len(r.reqs) == 0 || r.reqs[len(r.reqs)-1] != tc.want {
			t.Errorf("server connectors %s sent %q, want last %q", tc.args, r.reqs, tc.want)
		}
	}
	for _, args := range []string{"share c1 --all", "share c1", "rm c1"} {
		r.reqs = nil
		code, _, errw := cli(t, append([]string{"server", "connectors"}, strings.Fields(args)...)...)
		if code == 0 || len(r.reqs) != 0 || !strings.Contains(errw, "--yes") {
			t.Errorf("%s without --yes = %d %q, sent %q; want a refusal and no request", args, code, errw, r.reqs)
		}
	}
	if code, _, _ := cli(t, "server", "connectors", "share", "c1", "--all", "--org", "o1", "-y"); code != 2 {
		t.Errorf("--all with --org = %d, want a usage error", code)
	}
}
