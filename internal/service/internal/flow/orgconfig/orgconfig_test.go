package orgconfig_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
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
		Params: map[string]params.Value{
			"email.sender": {
				V: "old@acme.test",
			},
			"email.api_key": {
				V:      "k",
				Secret: true,
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
			file: v1 + `params:
  email:
    from:
      type: param
      value: noreply@acme.test
`,
			changes: []orgconfig.Change{
				{
					Kind:  "param",
					Field: "email.from",
				},
			},
		},
		{
			name: "param value changed",
			file: v1 + `params:
  email:
    sender:
      type: param
      value: new@acme.test
`,
			changes: []orgconfig.Change{
				{
					Kind:  "param",
					Field: "email.sender",
				},
			},
		},
		{
			name: "secret declared without a value is a note",
			file: v1 + `params:
  email:
    token:
      type: secret
`,
			note: "params.email.token is declared and not set",
		},
		{
			name: "a secret is never turned back into a param",
			file: v1 + `params:
  email:
    api_key:
      type: param
      value: k
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
    token:
      type: secret
      value: x
`,
			want: "its value never goes in the file",
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
	p := orgconfig.Plan{
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
		},
		Blockers: []string{
			"org: another organization already uses the slug \"globex\"",
		},
		Notes: []string{
			"params.email.token is declared and not set",
		},
	}
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
	if s := p.Summary(); s != "1 to add, 1 to change, 1 blocker" {
		t.Errorf("summary = %q", s)
	}
	if s := (&orgconfig.Plan{}).Summary(); s != "no changes" {
		t.Errorf("empty summary = %q", s)
	}
}
