package stackconf

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDep(t *testing.T) {
	s, c, err := runtime.ParseDep("db")
	require.NoError(t, err)
	assert.Equal(t, "db", s)
	assert.Equal(t, "started", c)
	s, c, err = runtime.ParseDep("db:healthy")
	require.NoError(t, err)
	assert.True(t, s == "db" && c == "healthy")
	_, _, err = runtime.ParseDep("db:sometimes")
	require.Error(t, err)
	_, _, err = runtime.ParseDep(":healthy")
	require.Error(t, err)
}

func TestValidateDeps(t *testing.T) {
	ok := map[string]TileConf{
		"db":      {Type: "managed", Engine: "postgres"},
		"init":    {Type: "function", RunOnDeploy: true},
		"web":     {Type: "service", DependsOn: []string{"db:healthy", "init:completed", "api"}},
		"api":     {Type: "service", DependsOn: []string{"db"}},
		"nightly": {Type: "cron"},
	}
	require.NoError(t, validateDeps("production", ok))

	bad := map[string]map[string]TileConf{
		"missing target": {"web": {Type: "service", DependsOn: []string{"ghost"}}},
		"self dep":       {"web": {Type: "service", DependsOn: []string{"web"}}},
		"completed on a service": {
			"api": {Type: "service"},
			"web": {Type: "service", DependsOn: []string{"api:completed"}},
		},
		"completed on a manual function": {
			"fn":  {Type: "function"},
			"web": {Type: "service", DependsOn: []string{"fn:completed"}},
		},
		"cycle": {
			"a": {Type: "service", DependsOn: []string{"b"}},
			"b": {Type: "service", DependsOn: []string{"c"}},
			"c": {Type: "service", DependsOn: []string{"a"}},
		},
	}
	for name, tiles := range bad {
		assert.Error(t, validateDeps("production", tiles), name)
	}
}

func TestTopoDeps(t *testing.T) {
	re := ResolvedEnv{Tiles: map[string]TileConf{
		"web":   {Type: "service", DependsOn: []string{"api", "cache"}},
		"api":   {Type: "service", DependsOn: []string{"cache"}},
		"cache": {Type: "service"},
		"solo":  {Type: "service"},
	}}
	got := topoDeps([]string{"web", "solo", "api", "cache"}, re)
	pos := map[string]int{}
	for i, s := range got {
		pos[s] = i
	}
	require.Len(t, got, 4)
	assert.Less(t, pos["cache"], pos["api"], "dependency must precede dependent: %v", got)
	assert.Less(t, pos["api"], pos["web"], "dependency must precede dependent: %v", got)

	// dep outside the set is ignored; cycle falls back to incoming order
	re2 := ResolvedEnv{Tiles: map[string]TileConf{
		"a": {Type: "service", DependsOn: []string{"b"}},
		"b": {Type: "service", DependsOn: []string{"a"}},
	}}
	got2 := topoDeps([]string{"a", "b"}, re2)
	assert.Equal(t, []string{"a", "b"}, got2, "cycle keeps incoming order")
}
