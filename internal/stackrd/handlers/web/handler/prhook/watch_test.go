package prhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWatchMatch(t *testing.T) {
	cases := []struct {
		name    string
		watch   string
		changed []string
		want    bool
	}{
		{"empty config", "", []string{"a.go"}, true},
		{"no file list", "^apps/", nil, true},
		{"positive hit", "^apps/web/", []string{"apps/web/main.go"}, true},
		{"positive miss", "^apps/web/", []string{"apps/api/main.go"}, false},
		{"ignore only, other file", "!^docs/", []string{"docs/readme.md", "main.go"}, true},
		{"ignore only, all ignored", "!^docs/", []string{"docs/readme.md"}, false},
		{"pos plus ignore", "^apps/\n!\\.md$", []string{"apps/web/readme.md"}, false},
		{"pos plus ignore, code file", "^apps/\n!\\.md$", []string{"apps/web/x.go"}, true},
		{"invalid regex skipped", "([\n^apps/", []string{"apps/x"}, true},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, watchMatch(c.watch, c.changed), "%s: watchMatch", c.name)
	}
}
