package envcolor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func TestResolveChain(t *testing.T) {
	envs := []repo.Environment{
		{ID: "d", Slug: "dev"},
		{ID: "s", Slug: "staging", Color: "rose"},
		{ID: "p", Slug: "production"},
		{ID: "pr", Slug: "pr-12", Type: "ephemeral"},
	}
	org := &repo.Org{EnvColors: `{"production":"#112233","staging":"lime"}`}

	m := Map(envs, org, false)
	assert.Equal(t, Resolved{Value: "violet", CSS: "var(--rw-env-violet)", Source: "default"}, m["d"], "dev is violet")
	assert.Equal(t, "rose", m["s"].Value, "env row beats org")
	assert.Equal(t, "stack", m["s"].Source)
	assert.Equal(t, "#112233", m["p"].Value, "org beats default")
	assert.Equal(t, "#112233", m["p"].CSS, "hex is used as is")
	assert.Equal(t, "teal", m["pr"].Value, "PR env takes the first hue no static holds")

	assert.Equal(t, "file", Map(envs, org, true)["s"].Source, "managed stack: the row colour is the file's")
	assert.Equal(t, "default", Map(envs, &repo.Org{EnvColors: `{"staging":"nope"}`}, false)["p"].Source, "bad org value is ignored")
}

func TestNamedDefaults(t *testing.T) {
	envs := []repo.Environment{
		{ID: "s", Slug: "staging"},
		{ID: "p", Slug: "production"},
		{ID: "q", Slug: "qa"},
		{ID: "x", Slug: "sandbox"},
	}
	m := Map(envs, nil, false)
	assert.Equal(t, "amber", m["s"].Value, "staging is amber whatever its rung")
	assert.Equal(t, "rose", m["p"].Value, "production is red")
	assert.Equal(t, "teal", m["q"].Value, "unnamed envs take the ladder from what is left")
	assert.Equal(t, "sky", m["x"].Value)
}

func TestValid(t *testing.T) {
	for _, ok := range []string{"", "teal", "#AbCdEf"} {
		assert.True(t, Valid(ok), ok)
	}
	for _, bad := range []string{"red", "#abc", "abcdef", "#gggggg"} {
		assert.False(t, Valid(bad), bad)
	}
}
