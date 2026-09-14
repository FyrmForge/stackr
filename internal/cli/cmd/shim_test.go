package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRewriteArgs(t *testing.T) {
	cases := [][2][]string{
		{{"tile", "web", "set", "--image", "x"}, {"tile", "set", "web", "--image", "x"}},
		{{"tile", "web", "rm"}, {"tile", "rm", "web"}},
		{{"tile", "--json", "web", "set"}, {"tile", "--json", "set", "web"}},
		{{"tile", "web", "domain", "add", "h.example"}, {"tile", "domain", "add", "web", "h.example"}},
		{{"tile", "web", "domain", "list"}, {"tile", "domain", "list", "web"}},
		{{"tile", "web", "volume", "rm", "v1"}, {"tile", "volume", "rm", "web", "v1"}},
		{{"tile", "create", "web"}, {"tile", "create", "web"}}, // canonical passes through
		{{"tile", "set", "web"}, {"tile", "set", "web"}},
		{{"apps", "web", "set"}, {"apps", "set", "web"}},
		{{"tile"}, {"tile"}},
		{{"login", "http://x", "-with-key"}, {"login", "http://x", "--with-key"}},
		{{"stack", "ls"}, {"stack", "ls"}},
		{{"tile", "--", "web", "set"}, {"tile", "--", "web", "set"}},
	}
	for _, c := range cases {
		assert.Equal(t, c[1], RewriteArgs(c[0]), "RewriteArgs(%v)", c[0])
	}
}
