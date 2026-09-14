package orgconf

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestParse(t *testing.T) {
	good := []byte(`
version: 1
org: vulpe
vars:
  BASE_DOMAIN: example.test
secrets:
  OIDC_SECRET: {default: generated}
shared:
  sharedpg: {type: managed, engine: postgres}
stacks:
  media:
    repo: you/media-stack
  infra:
    path: stacks/infra/stackr-compose.yml
  tiny:
    inline:
      version: 1
      stack: tiny
      base:
        tiles:
          web: {type: service, image: "nginx:1", port: 80}
    protected: true
`)
	f, err := Parse(good)
	require.NoError(t, err)
	assert.Equal(t, "vulpe", f.Org)
	assert.Equal(t, "repo", f.Stacks["media"].form())
	assert.Equal(t, "path", f.Stacks["infra"].form())
	assert.Equal(t, "inline", f.Stacks["tiny"].form())

	bad := map[string]string{
		"no version":     "org: x\n",
		"no org":         "version: 1\n",
		"formless stack": "version: 1\norg: x\nstacks:\n  a: {branch: main}\n",
		"inline+repo":    "version: 1\norg: x\nstacks:\n  a: {repo: r/r, inline: {version: 1, stack: a}}\n",
		"shared service": "version: 1\norg: x\nshared:\n  web: {type: service, image: n}\n",
		"broken inline":  "version: 1\norg: x\nstacks:\n  a: {inline: {version: 9}}\n",
	}
	for name, y := range bad {
		_, err := Parse([]byte(y))
		assert.Error(t, err, name)
	}
}

func TestDiff(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	r := Runner{Store: s}
	ctx := context.Background()

	f, err := Parse([]byte(`
version: 1
org: ` + seed.Org.Slug + `
vars:
  BASE_DOMAIN: example.test
shared:
  sharedpg: {type: managed, engine: postgres}
stacks:
  ` + seed.Stack.Slug + `:
    path: stacks/one/stackr-compose.yml
  newstack:
    repo: you/other
`))
	require.NoError(t, err)

	p, err := r.diff(ctx, seed.Org, f)
	require.NoError(t, err)
	kinds := map[string]int{}
	var fields []string
	for _, ch := range p.Changes {
		kinds[ch.Kind]++
		fields = append(fields, ch.Env+"/"+ch.Tile+"/"+ch.Field)
	}
	// var set + shared instance create + newstack create + existing stack
	// binding updates (repo/connector/branch/path as needed)
	assert.GreaterOrEqual(t, kinds["create"], 2, "expected shared + stack creates: %v", fields)
	assert.GreaterOrEqual(t, kinds["update"], 1, "expected var/binding updates: %v", fields)
	// The seeded clickops stack is declared, so nothing plans a delete.
	assert.Zero(t, kinds["delete"], "unexpected delete: %v", fields)
}

func TestDiff_OrgRename(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	r := Runner{Store: s}
	ctx := context.Background()

	f, err := Parse([]byte("version: 1\norg: renamed-org\n"))
	require.NoError(t, err)
	p, err := r.diff(ctx, seed.Org, f)
	require.NoError(t, err)
	require.Len(t, p.Changes, 1)
	assert.Equal(t, "slug", p.Changes[0].Field)
	assert.Equal(t, seed.Org.Slug, p.Changes[0].Old)
	assert.Equal(t, "renamed-org", p.Changes[0].New)

	// Renaming onto an existing org's slug is a plan error, not a change.
	taken := &repo.Org{ID: "org2", Name: "Taken", Slug: "renamed-org", CreatedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrg(ctx, taken))
	p, err = r.diff(ctx, seed.Org, f)
	require.NoError(t, err)
	assert.Empty(t, p.Changes)
	require.Len(t, p.Errors, 1)
	assert.Contains(t, p.Errors[0], "collides")
}

// markDeclared keeps each stack row's OrgDeclared flag tracking the file, in
// both directions, matching by slugified key, and the delete pass must use
// the same slug matching, or a display-name key ("Stack") reads as a
// deletion of the very stack it declares.
func TestMarkDeclaredAndSlugKeys(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true) // config-managed: eligible for the delete pass
	r := Runner{Store: s}

	f, err := Parse([]byte("version: 1\norg: org\nstacks:\n  Stack:\n    path: stackr-compose.yml\n"))
	require.NoError(t, err)
	r.markDeclared(ctx, seed.Org, f)
	st, err := s.GetStack(ctx, seed.Stack.ID)
	require.NoError(t, err)
	assert.True(t, st.OrgDeclared, "declared stack not flagged")

	p, err := r.diff(ctx, seed.Org, f)
	require.NoError(t, err)
	for _, ch := range p.Changes {
		assert.NotEqual(t, "delete", ch.Kind, "display-name key planned a delete: %+v", ch)
	}

	// Dropped from the file: the flag clears on the next plan.
	f, err = Parse([]byte("version: 1\norg: org\n"))
	require.NoError(t, err)
	r.markDeclared(ctx, seed.Org, f)
	st, err = s.GetStack(ctx, seed.Stack.ID)
	require.NoError(t, err)
	assert.False(t, st.OrgDeclared, "undeclared stack kept the flag")
}

// The flattened inline form drops the inline: wrapper, the stack body sits
// right under its name, marked by any stack-file top-level key. protected:
// still reads as the org-level knob, version: and stack: are inherited from
// the org file and the key, and a disagreeing stack: errors instead of
// smuggling in a second name.
func TestParseFlattenedInline(t *testing.T) {
	f, err := Parse([]byte(`
version: 1
org: vulpe
stacks:
  tiny:
    protected: true
    base:
      tiles:
        web:
          type: service
          image: nginx:1
          port: 80
`))
	require.NoError(t, err)
	ref := f.Stacks["tiny"]
	assert.Equal(t, "inline", ref.form(), "flattened body not read as inline")
	assert.True(t, ref.Protected, "protected not peeled from the body")
	assert.Equal(t, "tiny", ref.Inline["stack"], "stack: not defaulted from the key")
	assert.Equal(t, 1, ref.Inline["version"], "version: not inherited from the org file")

	_, err = Parse([]byte(`
version: 1
org: x
stacks:
  tiny:
    version: 1
    stack: other
    base:
      tiles:
        web:
          type: service
          image: nginx:1
          port: 80
`))
	require.Error(t, err, "disagreeing stack: accepted")
	assert.Contains(t, err.Error(), "the org key names it")

	// The wrapped form may now omit stack: too, the key fills it in.
	f, err = Parse([]byte(`
version: 1
org: x
stacks:
  tiny:
    inline:
      version: 1
      base:
        tiles:
          web:
            type: service
            image: nginx:1
            port: 80
`))
	require.NoError(t, err, "wrapped inline without stack:")
	assert.Equal(t, "tiny", f.Stacks["tiny"].Inline["stack"])
}
