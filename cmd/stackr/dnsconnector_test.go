package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// testKinds is what the recorder answers GET .../connector-kinds with: one
// DNS kind with a secret, and a kind that signs in on its own site.
const testKinds = `[{"name":"namecheap","label":"Namecheap","steps":[{"title":"Account","fields":[` +
	`{"name":"user","label":"Namecheap username"},{"name":"key","label":"API key","secret":true}]},` +
	`{"title":"Allow list","fields":[{"name":"client_ip","label":"Server IPv4","prefill":"server_ipv4"}]}]},` +
	`{"name":"signin","label":"Sign In","redirects":true,"steps":[{"title":"Sign in","out":true}]}]`

// The DNS connector verbs send what the API reads, at both levels; add
// takes the kind's declared fields, the secret from stdin and never on
// argv; take over needs the domain typed, and -y does not stand in for it.
func TestDNSConnectorVerbs(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	const s = "/api/v1/admin/dns-connectors"
	const o = "/api/v1/orgs/acme/dns-connectors"
	const b = "/api/v1/orgs/beta/dns-connectors"
	for _, tc := range []struct{ args, want string }{
		{"server connectors ls --dns", "GET " + s + " "},
		{"server connectors kinds", "GET /api/v1/admin/connector-kinds "},
		{"server connectors add namecheap --set user=jo --stdin key --set client_ip=203.0.113.7",
			"POST /api/v1/admin/connector-kinds/namecheap " + `{"client_ip":"203.0.113.7","key":"s3cret","user":"jo"}`},
		{"server connectors domains c1", "GET " + s + "/c1/domains "},
		{"server connectors manage c1 acme.dev -y", "PUT " + s + `/c1/domains/acme.dev {"org_ids":[]}`},
		{"server connectors manage c1 acme.dev --org o1 --org o2", "PUT " + s + `/c1/domains/acme.dev {"org_ids":["o1","o2"]}`},
		{"server connectors unmanage c1 acme.dev", "DELETE " + s + "/c1/domains/acme.dev "},
		{"server connectors takeover c1 acme.dev --confirm acme.dev", "POST " + s + `/c1/domains/acme.dev/takeover {"confirm":"acme.dev"}`},
		{"org connectors ls --dns", "GET " + o + " "},
		{"org connectors add namecheap --set user=jo --stdin key",
			"POST /api/v1/orgs/acme/connector-kinds/namecheap " + `{"client_ip":"","key":"s3cret","user":"jo"}`},
		{"org connectors add namecheap --set user=jo --stdin key --org beta",
			"POST /api/v1/orgs/beta/connector-kinds/namecheap " + `{"client_ip":"","key":"s3cret","user":"jo"}`},
		{"org connectors domains c1 --org beta", "GET " + b + "/c1/domains "},
		{"org connectors manage c1 acme.dev", "PUT " + o + `/c1/domains/acme.dev {"org_ids":[]}`},
		{"org connectors unmanage c1 acme.dev", "DELETE " + o + "/c1/domains/acme.dev "},
		{"org connectors takeover c1 acme.dev --confirm acme.dev", "POST " + o + `/c1/domains/acme.dev/takeover {"confirm":"acme.dev"}`},
		{"org connectors rm c1 --dns -y", "DELETE " + o + "/c1 "},
	} {
		r.reqs = nil
		var out, errw bytes.Buffer
		code := run(context.Background(), strings.Fields(tc.args), strings.NewReader("s3cret\n"), &out, &errw, false)
		if code != 0 {
			t.Errorf("%s = %d %s", tc.args, code, errw.String())
			continue
		}
		if len(r.reqs) == 0 || r.reqs[len(r.reqs)-1] != tc.want {
			t.Errorf("%s sent %q, want last %q", tc.args, r.reqs, tc.want)
		}
	}
	for _, args := range []string{
		"server connectors takeover c1 acme.dev -y",
		"org connectors takeover c1 acme.dev -y",
		"server connectors add namecheap --set user=jo --set key=s3cret", // a secret on argv
		"server connectors add namecheap --set nope=1",
		"server connectors add nope",
		"server connectors add signin",
		"server connectors manage c1 acme.dev",
	} {
		r.reqs = nil
		code, _, _ := cli(t, strings.Fields(args)...)
		if code == 0 || slicesHasWrite(r.reqs) {
			t.Errorf("%s = %d, sent %q; want a refusal and no write", args, code, r.reqs)
		}
	}
}

// slicesHasWrite says whether any request in reqs is not a GET.
func slicesHasWrite(reqs []string) bool {
	for _, r := range reqs {
		if !strings.HasPrefix(r, "GET ") {
			return true
		}
	}
	return false
}
