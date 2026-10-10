package orgconfig_test

import (
	"encoding/json"
	"go.yaml.in/yaml/v3"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func ptr[T any](v T) *T { return &v }

const v1 = "version: 1\norg: Acme\n"

// live is org acme: stack shop (bound) and a hand-made unbound stack
// legacy, two params, its own domain, a GitHub connector. Org globex holds
// a domain and a claim.
func live() orgconfig.Live {
	return orgconfig.Live{
		Org: store.Org{
			ID:           "o1",
			Name:         "Acme",
			Slug:         "acme",
			Settings:     `{"cpu_limit":1}`,
			EnvColors:    `{"prod":"red"}`,
			ConfigRepo:   "https://github.com/acme/infra",
			ConfigBranch: "main",
			ConfigPath:   "stackr-org.yml",
		},
		Orgs: []store.Org{
			{
				ID:   "o1",
				Slug: "acme",
			},
			{
				ID:   "o2",
				Slug: "globex",
			},
		},
		Claims: []org.Claim{
			{
				Host:  "initech.example.com",
				OrgID: "o2",
			},
		},
		Stacks: []orgconfig.StackLive{
			{
				Stack: store.Stack{
					ID:           "s1",
					Slug:         "shop",
					ConfigRepo:   "https://github.com/acme/shop",
					ConfigBranch: "main",
				},
			},
			{
				Stack: store.Stack{
					ID:   "s2",
					Slug: "legacy",
				},
			},
		},
		Domains: []store.DomainResource{
			{
				ID:    "dr1",
				Level: "org",
				OrgID: ptr("o1"),
				Host:  "acme.example.com",
			},
			{
				ID:    "dr2",
				Level: "org",
				OrgID: ptr("o2"),
				Host:  "cdn.example.com",
			},
		},
		Connectors: []store.Connector{
			{
				ID:   "cn1",
				Host: "github.com",
			},
		},
	}
}

func TestDiff(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		live    func(*orgconfig.Live)
		changes []orgconfig.Change
		blocker string // a substring of the one blocker; "" = none
		note    string // a substring of the one note; "" = none
	}{
		{
			name: "the file says nothing: no change to params, stacks, domains, defaults, colors",
			file: v1,
		},
		{
			name: "rename",
			file: "version: 1\norg: Acme Corp\n",
			changes: []orgconfig.Change{
				{
					Kind:  "org",
					Field: "name",
					Old:   "Acme",
					New:   "Acme Corp",
				},
			},
		},
		{
			name: "rename onto another org's slug",
			file: "version: 1\norg: Globex\n",
			changes: []orgconfig.Change{
				{
					Kind:  "org",
					Field: "name",
					Old:   "Acme",
					New:   "Globex",
				},
			},
			blocker: `already uses the slug "globex"`,
		},
		{
			name: "rename onto a slug another org's domain leads with",
			file: "version: 1\norg: Initech\n",
			changes: []orgconfig.Change{
				{
					Kind:  "org",
					Field: "name",
					Old:   "Acme",
					New:   "Initech",
				},
			},
			blocker: "initech.example.com leads with",
		},
		{
			name: "param created with its value",
			live: withParams,
			file: v1 + `params:
  email:
    all:
      from: noreply@acme.test
      sender: old@acme.test
      api_key: {type: secret}
`,
			changes: []orgconfig.Change{
				{
					Kind:  "param",
					Tile:  "all",
					Field: "email.from",
					New:   "noreply@acme.test",
				},
			},
		},
		{
			name: "param value changed",
			live: withParams,
			file: v1 + `params:
  email:
    all:
      sender: new@acme.test
      api_key: {type: secret}
`,
			changes: []orgconfig.Change{
				{
					Kind:  "param-update",
					Tile:  "all",
					Field: "email.sender",
					Old:   "old@acme.test",
					New:   "new@acme.test",
					Note:  "panel has old@acme.test, file says new@acme.test",
				},
			},
		},
		{
			name: "a param that becomes a secret carries no value",
			live: withParams,
			file: v1 + `params:
  email:
    all:
      sender: {type: secret}
      api_key: {type: secret}
`,
			changes: []orgconfig.Change{
				{
					Kind:  "param-update",
					Tile:  "all",
					Field: "email.sender",
					Note:  "becomes a secret",
				},
			},
		},
		{
			name: "secret declared without a value is a note",
			live: withParams,
			file: v1 + `params:
  email:
    all:
      token: {type: secret}
      sender: old@acme.test
      api_key: {type: secret}
`,
			note: "params.email.token (all) is declared and not set",
		},
		{
			name: "a secret is never turned back into a param",
			live: withParams,
			file: v1 + `params:
  email:
    all:
      api_key: k
      sender: old@acme.test
`,
			blocker: "never turned back",
		},
		{
			name: "defaults present and different",
			file: v1 + `defaults:
  cpu_limit: 2
`,
			changes: []orgconfig.Change{
				{
					Kind:  "defaults",
					Field: "defaults",
				},
			},
		},
		{
			name: "defaults present and the same",
			file: v1 + `defaults:
  cpu_limit: 1
`,
		},
		{
			name: "colors present and different",
			file: v1 + `env_colors:
  prod: blue
`,
			changes: []orgconfig.Change{
				{
					Kind:  "colors",
					Field: "env_colors",
					Old:   `{"prod":"red"}`,
					New:   `{"prod":"blue"}`,
				},
			},
		},
		{
			name: "stack create from owner/name",
			file: v1 + `stacks:
  blog:
    repo: acme/blog
`,
			changes: []orgconfig.Change{
				{
					Kind: "create",
					Tile: "blog",
					New:  "https://github.com/acme/blog",
					Note: "file stackr-compose.yml",
				},
			},
		},
		{
			name: "stack create from a path in the org repo",
			file: v1 + `stacks:
  blog:
    path: stacks/blog.yml
`,
			changes: []orgconfig.Change{
				{
					Kind: "create",
					Tile: "blog",
					New:  "https://github.com/acme/infra",
					Note: "file stacks/blog.yml",
				},
			},
		},
		{
			name: "a bound stack in the other spelling is no change",
			file: v1 + `stacks:
  shop:
    repo: acme/shop
    branch: main
    path: stackr-compose.yml
    connector: cn1
`,
		},
		{
			name: "rebind",
			file: v1 + `stacks:
  shop:
    repo: https://github.com/acme/shop
    branch: prod
`,
			changes: []orgconfig.Change{
				{
					Kind:  "rebind",
					Tile:  "shop",
					Field: "branch",
					Old:   "main",
					New:   "prod",
				},
			},
		},
		{
			name: "binding a hand-made stack is a rebind",
			file: v1 + `stacks:
  legacy:
    path: stacks/legacy.yml
`,
			changes: []orgconfig.Change{
				{
					Kind:  "rebind",
					Tile:  "legacy",
					Field: "repo",
					New:   "https://github.com/acme/infra",
				},
				{
					Kind:  "rebind",
					Tile:  "legacy",
					Field: "branch",
					New:   "main",
				},
				{
					Kind:  "rebind",
					Tile:  "legacy",
					Field: "path",
					Old:   "stackr-compose.yml",
					New:   "stacks/legacy.yml",
				},
				{
					Kind:  "rebind",
					Tile:  "legacy",
					Field: "connector",
					New:   "cn1",
				},
			},
		},
		{
			name: "stacks gone from the file, file-made and hand-made, are no change",
			file: v1 + `stacks:
  blog:
    repo: acme/blog
`,
			changes: []orgconfig.Change{
				{
					Kind: "create",
					Tile: "blog",
					New:  "https://github.com/acme/blog",
					Note: "file stackr-compose.yml",
				},
			},
		},
		{
			name: "host without a connector",
			file: v1 + `stacks:
  blog:
    repo: https://gitlab.example.com/acme/blog
`,
			changes: []orgconfig.Change{
				{
					Kind: "create",
					Tile: "blog",
					New:  "https://gitlab.example.com/acme/blog",
					Note: "file stackr-compose.yml",
				},
			},
			blocker: "no connected connector for https://gitlab.example.com/acme/blog",
		},
		{
			name: "two shared connectors on the host need a name",
			file: v1 + `stacks:
  blog:
    repo: acme/blog
`,
			live: func(l *orgconfig.Live) {
				l.Connectors = []store.Connector{{ID: "sv1", Host: "github.com"}, {ID: "sv2", Host: "github.com"}}
			},
			changes: []orgconfig.Change{
				{
					Kind: "create",
					Tile: "blog",
					New:  "https://github.com/acme/blog",
					Note: "file stackr-compose.yml",
				},
			},
			blocker: "name the connector to use",
		},
		{
			name: "a connector that is not the org's",
			file: v1 + `stacks:
  blog:
    repo: acme/blog
    connector: theirs
`,
			changes: []orgconfig.Change{
				{
					Kind: "create",
					Tile: "blog",
					New:  "https://github.com/acme/blog",
					Note: "file stackr-compose.yml",
				},
			},
			blocker: "connector theirs is not one of this org's",
		},
		{
			name: "path alone while the org is bound to no repo",
			file: v1 + `stacks:
  blog:
    path: stacks/blog.yml
`,
			live:    func(l *orgconfig.Live) { l.Org.ConfigRepo = "" },
			blocker: "bound to none",
		},
		{
			name: "moved stack renames, and its entry diffs under the new slug",
			file: v1 + `moved:
  - from: stack.shop
    to: stack.store
stacks:
  store:
    repo: acme/shop
    branch: main
`,
			changes: []orgconfig.Change{
				{
					Kind: "rename",
					Old:  "shop",
					New:  "store",
				},
			},
		},
		{
			name: "moved already applied is no change",
			file: v1 + `moved:
  - from: stack.weblog
    to: stack.shop
`,
		},
		{
			name: "moved with both ends existing",
			file: v1 + `moved:
  - from: stack.shop
    to: stack.legacy
`,
			blocker: "both exist",
		},
		{
			name: "moved with neither end existing",
			file: v1 + `moved:
  - from: stack.weblog
    to: stack.blog
`,
			blocker: "neither",
		},
		{
			name: "domain missing is a create",
			file: v1 + `domains:
  - host: shop.acme.io
`,
			changes: []orgconfig.Change{
				{
					Kind: "domain",
					New:  "shop.acme.io",
				},
			},
		},
		{
			name: "domain env flag and ACME differ",
			file: v1 + `domains:
  - host: acme.example.com
    include_env_on_default: true
    acme_email: ops@acme.test
`,
			changes: []orgconfig.Change{
				{
					Kind:  "domain-update",
					Tile:  "acme.example.com",
					Field: "include_env_on_default",
					Old:   "false",
					New:   "true",
				},
				{
					Kind:  "domain-update",
					Tile:  "acme.example.com",
					Field: "acme_email",
					New:   "ops@acme.test",
				},
			},
		},
		{
			name: "domain gone from the file is no change",
			file: v1 + `domains:
  - host: shop.acme.io
`,
			live: func(l *orgconfig.Live) {
				l.Domains = append(l.Domains, store.DomainResource{
					ID:    "dr3",
					Level: "org",
					OrgID: ptr("o1"),
					Host:  "shop.acme.io",
				})
			},
		},
		{
			name: "domain taken elsewhere",
			file: v1 + `domains:
  - host: cdn.example.com
`,
			blocker: "already a domain resource",
		},
		{
			name: "domain on an external route",
			file: v1 + `domains:
  - host: legacy.acme.io
`,
			live: func(l *orgconfig.Live) {
				l.Routes = []store.Route{{Host: "legacy.acme.io"}}
			},
			blocker: "external route",
		},
		{
			name: "domain squatting another org",
			file: v1 + `domains:
  - host: globex.acme.io
`,
			blocker: "another organization's slug",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := orgconfig.Parse([]byte(c.file))
			if err != nil {
				t.Fatal(err)
			}
			l := live()
			if c.live != nil {
				c.live(&l)
			}
			p := orgconfig.Diff(f, l)
			if !slices.Equal(p.Changes, c.changes) {
				t.Errorf("changes = %+v\nwant %+v", p.Changes, c.changes)
			}
			one(t, "blockers", p.Blockers, c.blocker)
			one(t, "notes", p.Notes, c.note)
		})
	}
}

// one checks got is empty when want is "", else one line containing want.
func one(t *testing.T, what string, got []string, want string) {
	t.Helper()
	switch {
	case want == "" && len(got) > 0:
		t.Errorf("%s = %q, want none", what, got)
	case want != "" && (len(got) != 1 || !strings.Contains(got[0], want)):
		t.Errorf("%s = %q, want one with %q", what, got, want)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		file string
		want string
	}{
		{
			name: "inline stack",
			file: v1 + `stacks:
  shop:
    tiles:
      web:
        image: nginx
`,
			want: "line 5: tiles: inline stacks are not supported; put the stack in its own file",
		},
		{
			name: "v0 inline key",
			file: v1 + `stacks:
  shop:
    repo: acme/shop
    inline:
      version: 1
`,
			want: "inline: inline stacks are not supported",
		},
		{
			name: "branch without repo",
			file: v1 + `stacks:
  shop:
    path: shop.yml
    branch: main
`,
			want: "branch and connector go with repo",
		},
		{
			name: "no shared section (DECIDE 193)",
			file: v1 + `shared:
  db:
    engine: postgres
`,
			want: "unknown key shared",
		},
		{
			name: "moved names stacks only",
			file: v1 + `moved:
  - from: shared.db
    to: shared.pg
`,
			want: `moved: "shared.db" is stack.<slug>`,
		},
		{
			name: "no org",
			file: "version: 1\n",
			want: "org: required",
		},
		{
			name: "wrong version",
			file: "version: 2\norg: Acme\n",
			want: "unsupported version 2",
		},
		{
			name: "a v0 key gets its hint",
			file: v1 + `vars:
  a: b
`,
			want: "unknown key vars (no longer supported: declare them under params:)",
		},
		{
			name: "a secret with a value",
			file: v1 + `params:
  email:
    all:
      token: {type: secret, value: x}
`,
			want: "unknown key value",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := orgconfig.Parse([]byte(c.file))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestPlanJSONAndSummary(t *testing.T) {
	p := orgconfig.Plan{Plan: planfile.Plan{
		Changes: []orgconfig.Change{
			{
				Kind: "create",
				Tile: "blog",
				New:  "https://github.com/acme/blog",
				Note: "file stackr-compose.yml",
			},
			{
				Kind:  "rebind",
				Tile:  "shop",
				Field: "branch",
				Old:   "main",
				New:   "prod",
			},
			{
				Kind:  "param",
				Field: "app.farewell",
				New:   "bye",
			},
			{
				Kind:  "param-update",
				Field: "app.greeting",
				New:   "hi",
			},
		},
		Blockers: []string{
			"org: another organization already uses the slug \"globex\"",
		},
		Notes: []string{
			"params.email.token (all) is declared and not set",
		},
	}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got orgconfig.Plan
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Errorf("round trip = %+v, want %+v", got, p)
	}
	if s := p.Summary(); s != "2 to add, 2 to change, 1 blocker" {
		t.Errorf("summary = %q", s)
	}
	if s := (&orgconfig.Plan{}).Summary(); s != "no changes" {
		t.Errorf("empty summary = %q", s)
	}
}

// A plan row stored before the shared planfile.Plan reads back whole, and
// marshals to the same bytes: the embed is flat in JSON.
func TestStoredPlanJSONUnchanged(t *testing.T) {
	const stored = `{"changes":[{"kind":"create","tile":"blog","new":"https://github.com/acme/blog"}],` +
		`"blockers":["b"],"notes":["n"]}`
	var p orgconfig.Plan
	if err := json.Unmarshal([]byte(stored), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || !p.Blocked() || p.Notes[0] != "n" {
		t.Fatalf("plan = %+v", p)
	}
	if b, err := json.Marshal(p); err != nil || string(b) != stored {
		t.Errorf("marshal = %s, %v; want %s", b, err, stored)
	}
}

func TestSummaryCountsRemovalRows(t *testing.T) {
	p := orgconfig.Plan{Plan: planfile.Plan{Changes: []orgconfig.Change{
		{Kind: "share", Tile: "media"},
		{Kind: "share-delete", Tile: "old", Key: "share:old", Optional: true},
	}}}
	if s := p.Summary(); s != "1 to add, 1 removal to review" {
		t.Errorf("summary = %q", s)
	}
}

// withParams gives the org two live org-wide params, one of them a secret.
func withParams(l *orgconfig.Live) {
	l.Params = map[string]params.Value{
		"email.sender":  {V: "old@acme.test"},
		"email.api_key": {V: "k", Secret: true},
	}
}

func TestParseTiersAndParams(t *testing.T) {
	tiers := "tiers:\n  dev: {locked: false}\n  staging: {locked: true}\n  prod:\n"
	f, err := orgconfig.Parse([]byte(v1 + tiers + "params:\n  smtp:\n    dev|staging:\n      host: h\n      pw: {type: secret}\n    prod:\n      pw: {type: secret, generate: 32}\n    pr:\n      host: sandbox\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Tiers; !reflect.DeepEqual(got, orgconfig.Tiers{{"dev", false}, {"staging", true}, {"prod", true}}) {
		t.Errorf("tiers = %+v", got)
	}
	if f.Params.Tiered["smtp"]["dev|staging"]["host"].Value != "h" {
		t.Errorf("params = %+v", f.Params)
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	g, err := orgconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !reflect.DeepEqual(f.Tiers, g.Tiers) || !reflect.DeepEqual(f.Params.Tiered, g.Params.Tiered) ||
		strings.Index(string(out), "dev:") > strings.Index(string(out), "staging:") {
		t.Errorf("round trip:\n%s", out)
	}

	// No tiers: the same shape with the env keys all and pr, and it round trips.
	w, err := orgconfig.Parse([]byte(v1 + "params:\n  smtp:\n    all:\n      host: h\n      pw: {type: secret}\n    pr:\n      host: sandbox\n"))
	if err != nil || w.Params.Tiered["smtp"]["all"]["host"].Value != "h" || w.Blocks()["pr"]["smtp.host"].Value != "sandbox" {
		t.Fatalf("untiered = %+v, %v", w.Params, err)
	}
	out, _ = yaml.Marshal(w)
	if w2, err := orgconfig.Parse(out); err != nil || !reflect.DeepEqual(w.Params.Tiered, w2.Params.Tiered) {
		t.Errorf("untiered round trip: %v\n%s", err, out)
	}
	if n, _ := yaml.Marshal(orgconfig.File{Version: 1, Org: "Acme"}); strings.Contains(string(n), "params") || strings.Contains(string(n), "tiers") {
		t.Errorf("empty file prints %s", n)
	}

	for name, c := range map[string]struct{ src, want string }{
		"unknown tier":  {tiers + "params:\n  a:\n    demo:\n      x: y\n", `"demo" is not a tier of this org`},
		"old shape":     {tiers + "params:\n  a:\n    x: {type: param, value: y}\n", `"x" is not a tier of this org`},
		"tier key none": {"params:\n  a:\n    dev:\n      x: y\n", `"dev" is not an env key of an org with no tiers`},
		"old wide":      {"params:\n  a:\n    x: {type: param, value: y}\n", `"x" is not an env key of an org with no tiers`},
		"all, tiered":   {tiers + "params:\n  a:\n    all:\n      x: y\n", "all is for an org with no tiers"},
		"all as tier":   {"tiers:\n  all: {}\n", "not a tier slug"},
		"conflict":      {tiers + "params:\n  a:\n    dev|prod:\n      x: y\n    prod:\n      x: z\n", `by both "dev|prod" and "prod"`},
		"pr tier":       {"tiers:\n  pr: {locked: true}\n", "pr and order are reserved"},
		"order tier":    {"tiers:\n  order: {}\n", "pr and order are reserved"},
		"tier twice":    {"tiers:\n  dev: {}\n  dev: {}\n", "declared twice"},
		"bad slug":      {"tiers:\n  Dev: {}\n", "not a tier slug"},
		"tier key":      {"tiers:\n  dev: {lock: true}\n", "unknown key lock"},
		"tiers a list":  {"tiers: [dev]\n", "tiers is a map"},
	} {
		if _, err := orgconfig.Parse([]byte(v1 + c.src)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// tiersLive is acme with tiers dev (open) and prod (locked), a stack env in
// prod, and values in both blocks and pr.
func tiersLive() orgconfig.Live {
	l := live()
	l.Tiers = []store.Tier{{ID: "t1", Slug: "dev", Position: 0}, {ID: "t2", Slug: "prod", Position: 1, Locked: true}}
	l.TierParams = map[string]map[string]params.Value{
		"dev":  {"smtp.host": {V: "d"}, "smtp.pw": {V: "x", Secret: true}},
		"prod": {"smtp.host": {V: "p"}},
	}
	l.PRParams = map[string]params.Value{"smtp.host": {V: "sandbox"}}
	l.TierUsers = map[string][]string{"prod": {"shop/prod"}}
	return l
}

func rowKinds(p orgconfig.Plan) string {
	var ks []string
	for _, c := range p.Changes {
		ks = append(ks, c.Kind+":"+c.Tile+c.Field+c.Old+">"+c.New)
	}
	return strings.Join(ks, " ")
}

func TestDiffTiers(t *testing.T) {
	parse := func(body string) *orgconfig.File {
		f, err := orgconfig.Parse([]byte(v1 + body))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	same := "tiers:\n  dev: {locked: false}\n  prod: {locked: true}\nparams:\n  smtp:\n    dev:\n      host: d\n      pw: {type: secret}\n    prod:\n      host: p\n    pr:\n      host: sandbox\n"
	if p := orgconfig.Diff(parse(same), tiersLive()); len(p.Changes) != 0 || p.Blocked() || len(p.Notes) != 0 {
		t.Errorf("same file: %s %v %v", rowKinds(p), p.Blockers, p.Notes)
	}

	for name, c := range map[string]struct{ file, want string }{
		"lock":   {strings.Replace(same, "dev: {locked: false}", "dev: {locked: true}", 1), "tier-lock:devunlocked>locked"},
		"order":  {strings.Replace(same, "  dev: {locked: false}\n  prod: {locked: true}\n", "  prod: {locked: true}\n  dev: {locked: false}\n", 1), "tier-order:dev, prod>prod, dev"},
		"create": {strings.Replace(same, "  prod: {locked: true}\n", "  prod: {locked: true}\n  qa: {locked: false}\n", 1), "tier:qa> tier-lock:qalocked>unlocked"},
		"drift":  {strings.Replace(same, "host: d", "host: new", 1), "param-update:devsmtp.hostd>new"},
		"add":    {same + "    dev|prod:\n      region: eu\n", "param:devsmtp.region>eu param:prodsmtp.region>eu"},
		"pr":     {strings.Replace(same, "host: sandbox", "host: other", 1), "param-update:prsmtp.hostsandbox>other"},
		"secret": {strings.Replace(same, "      host: p\n", "      host: p\n      pw: {type: secret}\n", 1), ""},
	} {
		p := orgconfig.Diff(parse(c.file), tiersLive())
		if got := rowKinds(p); got != c.want {
			t.Errorf("%s: rows = %q, want %q (blockers %v)", name, got, c.want, p.Blockers)
		}
	}

	// A dropped tier is a removal row, and a blocker while a stack env has its slug.
	devOnly := "tiers:\n  dev: {locked: false}\nparams:\n  smtp:\n    dev:\n      host: d\n      pw: {type: secret}\n    pr:\n      host: sandbox\n"
	p := orgconfig.Diff(parse(devOnly), tiersLive())
	if len(p.Removals()) != 1 || p.Removals()[0].Key != "tier:prod" || len(p.Blockers) != 1 ||
		!strings.Contains(p.Blockers[0], "shop/prod") {
		t.Errorf("drop prod: rows %s, blockers %v", rowKinds(p), p.Blockers)
	}
	l := tiersLive()
	l.TierUsers = nil
	if p := orgconfig.Diff(parse(devOnly), l); p.Blocked() || len(p.Removals()) != 1 {
		t.Errorf("drop unused prod: %s %v", rowKinds(p), p.Blockers)
	}

	// The plan that adds the first tier warns; tier-less files keep the wide shape.
	l = live()
	p = orgconfig.Diff(parse("tiers:\n  dev:\n"), l)
	if !slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "org-wide params stop being read") }) ||
		rowKinds(p) != "tier:dev>" {
		t.Errorf("first tier: %s %v", rowKinds(p), p.Notes)
	}
	// Unlocks, unlocked new tiers and renames carry an impact, so none is auto;
	// locking carries none.
	ladder := "tiers:\n  dev: {locked: false}\n  prod: {locked: true}\n"
	keep := "params:\n  smtp:\n    dev:\n      host: d\n      pw: {type: secret}\n    prod:\n      host: p\n    pr:\n      host: sandbox\n"
	for name, c := range map[string]struct {
		file   string
		impact bool
	}{
		"unlock":     {strings.Replace(ladder, "prod: {locked: true}", "prod: {locked: false}", 1), true},
		"new open":   {ladder + "  qa: {locked: false}\n", true},
		"lock":       {strings.Replace(ladder, "dev: {locked: false}", "dev: {locked: true}", 1), false},
		"new locked": {ladder + "  qa: {locked: true}\n", false},
	} {
		if p := orgconfig.Diff(parse(c.file+keep), tiersLive()); p.AutoOK() == c.impact {
			t.Errorf("%s: AutoOK = %v, rows %s", name, p.AutoOK(), rowKinds(p))
		}
	}
	// No tiers: block drops every live tier, blocked while stack envs carry the slug.
	for _, body := range []string{"", "tiers:\n"} {
		p = orgconfig.Diff(parse(body), tiersLive())
		if len(p.Removals()) != 2 || len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "shop/prod") ||
			!slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "read again") }) {
			t.Errorf("no tiers %q: rows %s, blockers %v, notes %v", body, rowKinds(p), p.Blockers, p.Notes)
		}
	}
	l = tiersLive()
	l.TierUsers = nil
	if p = orgconfig.Diff(parse(""), l); p.Blocked() || len(p.Removals()) != 2 {
		t.Errorf("no tiers, none in use: %s %v", rowKinds(p), p.Blockers)
	}
	// A param a tiered file does not set is a removal row: plain applies with
	// the plan, a secret needs a tick; a dropped tier's params go with it; the
	// org scope is not read by a tiered org, so it is not managed.
	l = tiersLive()
	l.Params = map[string]params.Value{"x.y": {V: "z"}}
	p = orgconfig.Diff(parse(strings.Replace(same, "      pw: {type: secret}\n", "", 1)), l)
	if got := rowKinds(p); got != "param-remove:devsmtp.pw>" || len(p.Removals()) != 1 || p.Removals()[0].Key != "param-remove:dev:smtp.pw" {
		t.Errorf("tiered removal rows = %q", got)
	}
	p = orgconfig.Diff(parse(strings.Replace(same, "    pr:\n      host: sandbox\n", "", 1)), tiersLive())
	if got := rowKinds(p); got != "param-remove:prsmtp.host>" || len(p.Removals()) != 0 || !p.AutoOK() {
		t.Errorf("plain removal = %q, optional %d", got, len(p.Removals()))
	}
	// A tier rename is a blocker while a stack env carries the old slug.
	p = orgconfig.Diff(parse("tiers:\n  dev: {locked: false}\n  live: {locked: true}\nmoved:\n  - from: tier.prod\n    to: tier.live\n"), tiersLive())
	if len(p.Blockers) == 0 || !strings.Contains(p.Blockers[0], "shop/prod") {
		t.Errorf("rename in use: blockers %v", p.Blockers)
	}
	// A moved: tier rename keeps the tier's values.
	p = orgconfig.Diff(parse("tiers:\n  local: {locked: false}\n  prod: {locked: true}\nparams:\n  smtp:\n    local:\n      host: d\n      pw: {type: secret}\n    prod:\n      host: p\n    pr:\n      host: sandbox\nmoved:\n  - from: tier.dev\n    to: tier.local\n"), tiersLive())
	if got := rowKinds(p); got != "tier-rename:dev>local" {
		t.Errorf("rename rows = %q", got)
	}
}

// An org with no tiers uses all (the org scope) and pr (org_pr); a name the
// file leaves out is removed from either, a secret only when ticked.
func TestDiffUntiered(t *testing.T) {
	parse := func(body string) *orgconfig.File {
		f, err := orgconfig.Parse([]byte(v1 + body))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	l := live()
	withParams(&l)
	l.PRParams = map[string]params.Value{"email.sender": {V: "sandbox"}, "email.pw": {V: "s", Secret: true}}
	same := "params:\n  email:\n    all:\n      sender: old@acme.test\n      api_key: {type: secret}\n    pr:\n      sender: sandbox\n      pw: {type: secret}\n"
	if p := orgconfig.Diff(parse(same), l); len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("same file: %s %v", rowKinds(p), p.Blockers)
	}
	p := orgconfig.Diff(parse(strings.Replace(strings.Replace(same, "sender: old@acme.test", "sender: new\n      region: eu", 1), "      sender: sandbox\n", "", 1)), l)
	if got := rowKinds(p); got != "param:allemail.region>eu param-update:allemail.senderold@acme.test>new param-remove:premail.sender>" &&
		got != "param-update:allemail.senderold@acme.test>new param:allemail.region>eu param-remove:premail.sender>" {
		t.Errorf("rows = %q", got)
	}
	// nothing set: plain names go with the plan, secrets wait for a tick
	p = orgconfig.Diff(parse(""), l)
	var keys []string
	for _, c := range p.Removals() {
		keys = append(keys, c.Key)
	}
	slices.Sort(keys)
	if len(p.Changes) != 4 || !slices.Equal(keys, []string{"param-remove:all:email.api_key", "param-remove:pr:email.pw"}) || p.AutoOK() {
		t.Errorf("empty file: %s keys %v", rowKinds(p), keys)
	}
	if got := p.OnlyTicked(nil); len(got.Changes) != 2 {
		t.Errorf("unticked apply keeps %d rows, want the 2 plain", len(got.Changes))
	}
}

// Export of an untiered org writes all and pr, and diffing it back is clean.
func TestExportUntieredParams(t *testing.T) {
	l := live()
	withParams(&l)
	l.PRParams = map[string]params.Value{"email.sender": {V: "sandbox"}}
	out, err := orgconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "all:") || !strings.Contains(string(out), "pr:") {
		t.Errorf("export lacks all/pr:\n%s", out)
	}
	f, err := orgconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p := orgconfig.Diff(f, l); len(p.Changes) != 0 {
		t.Errorf("export diffs: %s", rowKinds(p))
	}
}
