package avatar

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The storage backend may be a plain filesystem, so a URL path reaching it
// unchecked is a directory-traversal read of anything the process can open.
func TestCleanRejectsEscapes(t *testing.T) {
	bad := []string{
		"",
		"../../etc/passwd",
		"users/../../etc/passwd",
		"/etc/passwd",
		"secrets/key.png",      // not one of our two prefixes
		"users/nested/foo.png", // deeper than we ever write
	}
	for _, in := range bad {
		assert.Equal(t, "", clean(in), "clean(%q): want rejected", in)
	}

	good := map[string]string{
		"users/abc-1234.png": "users/abc-1234.png",
		"/orgs/xyz-9876.jpg": "orgs/xyz-9876.jpg",
	}
	for in, want := range good {
		assert.Equal(t, want, clean(in), "clean(%q)", in)
	}
}

func TestURLEmptyStaysEmpty(t *testing.T) {
	assert.Equal(t, "", URL(""), "an unset avatar path must not produce a URL")
	assert.Equal(t, "/avatars/users/a.png", URL("users/a.png"), "unexpected URL")
}
