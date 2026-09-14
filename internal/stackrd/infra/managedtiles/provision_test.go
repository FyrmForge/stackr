package managedtiles

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func TestSQLIdent(t *testing.T) {
	cases := map[string]string{
		"my-api":    "my_api",
		"Web App":   "webapp",
		"9lives":    "db_9lives",
		"---":       "___", // ugly but valid + deterministic
		"ok_name_2": "ok_name_2",
	}
	for in, want := range cases {
		assert.Equal(t, want, sqlIdent(in), "sqlIdent(%q)", in)
	}
}

// The separator is the engine's: "_" keeps an SQL identifier legal, "-" keeps
// a bucket name DNS-safe.
func TestUniqueSliceName(t *testing.T) {
	existing := []repo.Provision{{DBName: "api"}, {DBName: "api_2"}}
	assert.Equal(t, "api_3", uniqueSliceName("api", existing, "_"))
	assert.Equal(t, "web", uniqueSliceName("web", existing, "_"))
	buckets := []repo.Provision{{DBName: "assets"}}
	assert.Equal(t, "assets-2", uniqueSliceName("assets", buckets, "-"))
}

func TestInfraPath(t *testing.T) {
	cases := []struct {
		scope, want string
	}{
		{"org", "acme:cache"},
		{"stack", "acme:shop:cache"},
		{"env", "acme:shop:prod:cache"},
		{"", "acme:shop:prod:cache"}, // env default
	}
	for _, c := range cases {
		assert.Equal(t, c.want, InfraPath(c.scope, "acme", "shop", "prod", "cache"), "InfraPath(%q)", c.scope)
	}
}
