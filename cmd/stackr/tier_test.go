package main

import (
	"strings"
	"testing"
)

// The tier verbs, env lock and the --tier / --pr param scopes reach their routes.
func TestTierRoutes(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	const org = "/api/v1/orgs/acme"
	for args, want := range map[string]string{
		"tier add qa":                      "POST " + org + `/tiers {"slug":"qa"}`,
		"tier rename qa test":              "PUT " + org + `/tiers/qa {"slug":"test"}`,
		"tier order dev prod":              "PUT " + org + `/tiers/order {"slugs":["dev","prod"]}`,
		"tier lock prod":                   "PUT " + org + `/tiers/prod/lock {"locked":true}`,
		"tier unlock prod":                 "PUT " + org + `/tiers/prod/lock {"locked":false}`,
		"tier rm qa -y":                    "DELETE " + org + "/tiers/qa ",
		"env lock --stack shop --env demo": "PUT " + org + `/stacks/shop/envs/demo/lock {"locked":true}`,
		"env unlock --stack shop --env x":  "PUT " + org + `/stacks/shop/envs/x/lock {"locked":false}`,
		"params get --tier prod":           "GET " + org + "/tiers/prod/params ",
		"params get --pr --stack shop":     "GET " + org + "/stacks/shop/pr/params ",
		"params get --pr --level org":      "GET " + org + "/pr/params ",
		"params get --tier prod --reveal":  "GET " + org + "/tiers/prod/params/secrets ",
	} {
		r.reqs = nil
		if code, _, errw := cli(t, strings.Fields(args)...); code != 0 {
			t.Errorf("%s = %d %s", args, code, errw)
		}
		if len(r.reqs) != 1 || r.reqs[0] != want {
			t.Errorf("%s sent %q, want %q", args, r.reqs, want)
		}
	}
	r.reqs = nil
	if code, _, _ := cli(t, "tier", "rm", "qa"); code == 0 || len(r.reqs) != 0 {
		t.Errorf("tier rm without -y = %d, sent %q", code, r.reqs)
	}
	for _, args := range []string{
		"params get --pr", "params get --tier prod --pr", "params get --tier prod --env dev",
		"params get --pr --level env", "params get --tier prod --level env",
	} {
		r.reqs = nil
		if code, _, _ := cli(t, strings.Fields(args)...); code == 0 || len(r.reqs) != 0 {
			t.Errorf("%s = %d, sent %q; want a refusal", args, code, r.reqs)
		}
	}
	if code, _, _ := cli(t, "volume", "ls", "--tier", "prod"); code == 0 {
		t.Error("volume ls took --tier")
	}
	r.reqs = nil
	cli(t, "params", "set", "db.host=h", "--tier", "prod")
	if want := "PATCH " + org + `/tiers/prod/params [{"collection":"db","kind":"param","name":"host","value":"h"}]`; len(r.reqs) != 1 || r.reqs[0] != want {
		t.Errorf("params set --tier sent %q, want %q", r.reqs, want)
	}
}
