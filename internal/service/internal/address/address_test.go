package address

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

const org = "testorg"

func addr(stack, env, tile string) Address {
	return Address{
		Org:   org,
		Stack: stack,
		Env:   env,
		Tile:  tile,
	}
}

func otherOrg(stack, env, tile string) Address {
	a := addr(stack, env, tile)
	a.Org = "otherorg"
	return a
}

func TestString(t *testing.T) {
	if got := addr("shop", "staging", "api").String(); got != "testorg:shop:staging:api" {
		t.Errorf("String() = %q", got)
	}
}

func TestParseAllowRefuses(t *testing.T) {
	for entry, want := range map[string]string{
		// own org only (DECIDE 194 call 2)
		"*":                     "allow: *: the first segment must be this org, testorg",
		"*:shop:*":              "allow: *:shop:*: the first segment must be this org, testorg",
		"otherorg:*":            "allow: otherorg:*: the first segment must be this org, testorg",
		"otherorg:shop:prod:db": "allow: otherorg:shop:prod:db: the first segment must be this org, testorg",
		// segment counts
		"testorg":           "allow: testorg: four segments or a trailing *",
		"testorg:shop":      "allow: testorg:shop: four segments or a trailing *",
		"testorg:shop:prod": "allow: testorg:shop:prod: four segments or a trailing *",
		"testorg:*:prod":    "allow: testorg:*:prod: four segments or a trailing *",
		"testorg:a:b:c:d":   "allow: testorg:a:b:c:d: more than four segments",
		"testorg:a:b:c:*":   "allow: testorg:a:b:c:*: more than four segments",
		"testorg:a:b:c:d:e": "allow: testorg:a:b:c:d:e: more than four segments",
		// empty segments
		"":                   "allow: : a segment is empty",
		":shop:*":            "allow: :shop:*: a segment is empty",
		"testorg::*":         "allow: testorg::*: a segment is empty",
		"testorg:shop:prod:": "allow: testorg:shop:prod:: a segment is empty",
		"testorg:*:":         "allow: testorg:*:: a segment is empty",
		// slugs: the tile slug is a DNS alias, so "_" is out (DECIDE 5 a);
		// darthvader's tile_b example is written tile-b below
		"testorg:stacka:*:tile_b": `allow: testorg:stacka:*:tile_b: "tile_b" is not a slug (lower-case letters, digits and single hyphens) or *`,
		"testorg:Shop:*":          `allow: testorg:Shop:*: "Shop" is not a slug (lower-case letters, digits and single hyphens) or *`,
		"testorg:api-*":           `allow: testorg:api-*: "api-*" is not a slug (lower-case letters, digits and single hyphens) or *`,
		"testorg:shop:**":         `allow: testorg:shop:**: "**" is not a slug (lower-case letters, digits and single hyphens) or *`,
	} {
		_, err := ParseAllow(org, []string{entry})
		if err == nil {
			t.Errorf("ParseAllow(%q) = nil error, want %q", entry, want)
			continue
		}
		if v, ok := errs.IsInvalid(err); !ok || v.Field != "allow" {
			t.Errorf("ParseAllow(%q) = %#v, want errs.Invalid on allow", entry, err)
		}
		if err.Error() != want {
			t.Errorf("ParseAllow(%q)\n got %q\nwant %q", entry, err.Error(), want)
		}
	}
}

func TestParseAllowOneBadEntryFailsTheList(t *testing.T) {
	ps, err := ParseAllow(org, []string{
		"testorg:*",
		"otherorg:*",
	})
	if err == nil || ps != nil {
		t.Errorf("ParseAllow = %v, %v; want nil and an error", ps, err)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		addr  Address
		want  bool
	}{
		// DECIDE 194, darthvader's examples
		{"org wide", []string{"testorg:*"}, addr("stacka", "production", "tile-b"), true},
		{"org wide, any stack", []string{"testorg:*"}, addr("blog", "dev", "api"), true},
		{"org wide, other org", []string{"testorg:*"}, otherOrg("stacka", "production", "tile-b"), false},
		{"one env of a stack", []string{"testorg:stacka:production:*"}, addr("stacka", "production", "anything"), true},
		{"one env of a stack, other env", []string{"testorg:stacka:production:*"}, addr("stacka", "staging", "tile-b"), false},
		{"one env of a stack, other stack", []string{"testorg:stacka:production:*"}, addr("stackb", "production", "tile-b"), false},
		{"one tile in every env", []string{"testorg:stacka:*:tile-b"}, addr("stacka", "production", "tile-b"), true},
		{"one tile in every env, staging", []string{"testorg:stacka:*:tile-b"}, addr("stacka", "staging", "tile-b"), true},
		{"one tile in every env, other tile", []string{"testorg:stacka:*:tile-b"}, addr("stacka", "production", "tile-c"), false},
		{"one tile in every env, other stack", []string{"testorg:stacka:*:tile-b"}, addr("stackb", "production", "tile-b"), false},
		// the step-7b plan's infra file
		{"plan shop", []string{"testorg:shop:*", "testorg:blog:*:api"}, addr("shop", "staging", "api-db"), true},
		{"plan blog api", []string{"testorg:shop:*", "testorg:blog:*:api"}, addr("blog", "dev", "api"), true},
		{"plan blog worker", []string{"testorg:shop:*", "testorg:blog:*:api"}, addr("blog", "dev", "worker"), false},
		{"plan other stack", []string{"testorg:shop:*", "testorg:blog:*:api"}, addr("infra", "staging", "api"), false},
		// trailing star
		{"trailing star after stack", []string{"testorg:shop:*"}, addr("shop", "production", "api"), true},
		{"trailing star after env", []string{"testorg:shop:prod:*"}, addr("shop", "prod", "api"), true},
		{"trailing star after env, other env", []string{"testorg:shop:prod:*"}, addr("shop", "dev", "api"), false},
		{"four stars", []string{"testorg:*:*:*"}, addr("shop", "prod", "api"), true},
		// middle star
		{"middle star, stack", []string{"testorg:*:prod:api"}, addr("blog", "prod", "api"), true},
		{"middle star, stack, other env", []string{"testorg:*:prod:api"}, addr("blog", "dev", "api"), false},
		{"two middle stars", []string{"testorg:*:*:api"}, addr("blog", "dev", "api"), true},
		{"two middle stars, other tile", []string{"testorg:*:*:api"}, addr("blog", "dev", "web"), false},
		// exact
		{"exact", []string{"testorg:shop:prod:api"}, addr("shop", "prod", "api"), true},
		{"exact, other tile", []string{"testorg:shop:prod:api"}, addr("shop", "prod", "web"), false},
		{"exact, other env", []string{"testorg:shop:prod:api"}, addr("shop", "dev", "api"), false},
		{"exact, other stack", []string{"testorg:shop:prod:api"}, addr("blog", "prod", "api"), false},
		{"exact, other org", []string{"testorg:shop:prod:api"}, otherOrg("shop", "prod", "api"), false},
	}
	for _, c := range cases {
		ps, err := ParseAllow(org, c.allow)
		if err != nil {
			t.Fatalf("%s: ParseAllow(%v): %v", c.name, c.allow, err)
		}
		if got := Match(ps, c.addr); got != c.want {
			t.Errorf("%s: Match(%v, %s) = %v, want %v", c.name, c.allow, c.addr, got, c.want)
		}
	}
}

func TestMatchNoPatterns(t *testing.T) {
	if Match(nil, addr("shop", "prod", "api")) {
		t.Error("Match(nil) admitted an address")
	}
}

func TestAllowed(t *testing.T) {
	self := addr("infra", "staging", "pg-db")
	cases := []struct {
		name  string
		allow []string
		addr  Address
		want  bool
	}{
		// no list = own env, any tile in it
		{"nil, same env", nil, addr("infra", "staging", "api"), true},
		{"nil, itself", nil, self, true},
		{"nil, other env", nil, addr("infra", "production", "api"), false},
		{"nil, other stack", nil, addr("shop", "staging", "api"), false},
		{"nil, other org", nil, otherOrg("infra", "staging", "api"), false},
		{"empty, same env", []string{}, addr("infra", "staging", "api"), true},
		{"empty, other env", []string{}, addr("infra", "production", "api"), false},
		// a list adds to the own-env default
		{"list, listed", []string{"testorg:shop:*"}, addr("shop", "staging", "api-db"), true},
		{"list, own env not listed", []string{"testorg:shop:*"}, addr("infra", "staging", "api"), true},
		{"list, other env not listed", []string{"testorg:shop:*"}, addr("infra", "production", "api"), false},
	}
	for _, c := range cases {
		got, err := Allowed(org, c.allow, self, c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: Allowed(%v, %s) = %v, want %v", c.name, c.allow, c.addr, got, c.want)
		}
	}
}

func TestAllowedBadList(t *testing.T) {
	ok, err := Allowed(org, []string{"*"}, addr("infra", "staging", "pg-db"), addr("infra", "staging", "api"))
	if _, isInvalid := errs.IsInvalid(err); ok || !isInvalid {
		t.Errorf("Allowed(*) = %v, %v; want false and errs.Invalid", ok, err)
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]Target{
		"infra:staging:pg-db": {
			Stack: "infra",
			Env:   "staging",
			Tile:  "pg-db",
		},
		"infra:${{ env.name }}:pg-db": {
			Stack: "infra",
			Env:   "${{ env.name }}",
			Tile:  "pg-db",
		},
		"${{ params.infra.stack }}:${{env.name}}:${{ params.infra.db }}": {
			Stack: "${{ params.infra.stack }}",
			Env:   "${{env.name}}",
			Tile:  "${{ params.infra.db }}",
		},
	} {
		got, err := ParseTarget(in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestParseTargetRefuses(t *testing.T) {
	for in, want := range map[string]string{
		// segment count
		"":                            "provision_from: : three segments, <stack>:<env>:<tile>",
		"pg-db":                       "provision_from: pg-db: three segments, <stack>:<env>:<tile>",
		"staging:pg-db":               "provision_from: staging:pg-db: three segments, <stack>:<env>:<tile>",
		"testorg:infra:staging:pg-db": "provision_from: testorg:infra:staging:pg-db: three segments, <stack>:<env>:<tile>",
		// empty segments
		"::":             "provision_from: ::: a segment is empty",
		"infra::pg-db":   "provision_from: infra::pg-db: a segment is empty",
		":staging:pg-db": "provision_from: :staging:pg-db: a segment is empty",
		"infra:staging:": "provision_from: infra:staging:: a segment is empty",
		// not a slug
		"infra:staging:pg_db": `provision_from: infra:staging:pg_db: "pg_db" is not a slug (lower-case letters, digits and single hyphens)`,
		"infra:*:pg-db":       `provision_from: infra:*:pg-db: "*" is not a slug (lower-case letters, digits and single hyphens)`,
		"Infra:staging:pg-db": `provision_from: Infra:staging:pg-db: "Infra" is not a slug (lower-case letters, digits and single hyphens)`,
	} {
		_, err := ParseTarget(in)
		if err == nil {
			t.Errorf("ParseTarget(%q) = nil error, want %q", in, want)
			continue
		}
		if v, ok := errs.IsInvalid(err); !ok || v.Field != "provision_from" {
			t.Errorf("ParseTarget(%q) = %#v, want errs.Invalid on provision_from", in, err)
		}
		if err.Error() != want {
			t.Errorf("ParseTarget(%q)\n got %q\nwant %q", in, err.Error(), want)
		}
	}
}
