package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Protecting preview URLs only works when the browser actually sends the
// panel's session cookie to them. Get this wrong and the feature doesn't
// degrade, every preview URL becomes an unreachable redirect loop.
func TestCookieCovers(t *testing.T) {
	cases := []struct {
		name         string
		cookie, base string
		want         bool
	}{
		{"panel is a parent of the base", "example.com", "apps.example.com", true},
		{"panel domain is the base itself", "apps.example.com", "apps.example.com", true},
		{"leading dot is still a parent", ".example.com", "apps.example.com", true},
		{"unrelated domains", "panel.example.com", "apps.other.com", false},
		{"panel is a sibling, not a parent", "panel.example.com", "apps.example.com", false},
		{"suffix match must respect the dot", "ample.com", "apps.example.com", false},
		{"no base configured is nothing to protect", "example.com", "", true},
		// No BASE_URL means a host-only cookie for whatever hostname the panel
		// answered on, which a preview URL never matches. That is the lockout
		// case, so it has to warn, it used to be the one case that stayed quiet.
		{"no cookie domain set cannot cover a base", "", "apps.example.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, CookieCovers(c.cookie, c.base), "CookieCovers(%q, %q)", c.cookie, c.base)
		})
	}
}

// Two nodes on the same host answer in well under a millisecond, and a bare
// "0 ms" in the node table reads as "never measured" rather than "fast".
func TestPingText(t *testing.T) {
	assert.Equal(t, "<1 ms", pingText(0))
	assert.Equal(t, "<1 ms", pingText(0.4))
	assert.Equal(t, "1 ms", pingText(1))
	assert.Equal(t, "142 ms", pingText(141.6))
}
