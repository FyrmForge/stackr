package prhook

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func TestNormalizeRepo(t *testing.T) {
	want := "github.com/acme/shop"
	for _, u := range []string{
		"https://github.com/acme/shop.git",
		"git@github.com:acme/shop.git",
		"ssh://git@github.com/acme/shop",
		"GitHub.com/Acme/Shop",
	} {
		assert.Equal(t, want, normalizeRepo(u), "normalizeRepo(%q)", u)
	}
	assert.True(t, sameRepo("git@github.com:acme/shop.git", "https://github.com/acme/shop.git", "", "acme/shop"),
		"sameRepo should match ssh vs https forms")
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"x":1}`)
	// echo -n '{"x":1}' | openssl dgst -sha256 -hmac s3cret
	sig := "sha256=75e31067a7b58ac9207ca9b950a2104dbc31159e3dc2f2881ffd48f614e8786c"
	assert.True(t, validSignature("s3cret", sig, body), "valid signature rejected")
	assert.False(t, validSignature("s3cret", "sha256=deadbeef", body), "bad signature accepted")
	assert.False(t, validSignature("", sig, body), "empty secret must never validate")
}

func TestConfigOnlyPush(t *testing.T) {
	bound := &repo.Stack{ConfigConnectorID: "c1", ConfigRepo: "o/r"}
	cases := []struct {
		name    string
		s       *repo.Stack
		changed []string
		want    bool
	}{
		{"only config", bound, []string{"stackr-compose.yml"}, true},
		{"config plus code", bound, []string{"stackr-compose.yml", "main.go"}, false},
		{"empty commit list", bound, nil, false},
		{"unbound stack", &repo.Stack{}, []string{"stackr-compose.yml"}, false},
		{"custom path", &repo.Stack{ConfigConnectorID: "c1", ConfigRepo: "o/r", ConfigPath: "deploy/s.yml"}, []string{"deploy/s.yml"}, true},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, configOnlyPush(c.s, c.changed), "%s", c.name)
	}
}
