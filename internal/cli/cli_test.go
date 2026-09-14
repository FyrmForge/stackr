package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, err := Load()
	require.ErrorIs(t, err, ErrNotLoggedIn, "Load on empty = %v, want ErrNotLoggedIn", err)

	want := Config{URL: "http://localhost:3000", Key: "sk_abc"}
	require.NoError(t, Save(want), "Save")
	got, err := Load()
	require.NoError(t, err, "Load")
	assert.Equal(t, want, got, "round-trip = %+v, want %+v", got, want)

	require.NoError(t, Clear(), "Clear")
	_, err = Load()
	assert.ErrorIs(t, err, ErrNotLoggedIn, "Load after Clear = %v, want ErrNotLoggedIn", err)
	// Clear on an absent file is not an error.
	assert.NoError(t, Clear(), "Clear (absent) should be nil")
}

func TestLinkWalkUpAndRemove(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	want := Link{Stack: "p1", StackName: "web", Env: "e1", EnvName: "prod"}
	require.NoError(t, SaveLink(root, want), "SaveLink")

	t.Chdir(sub) // FindLink resolves from cwd; must walk up to root
	got, _, err := FindLink()
	require.NoError(t, err, "FindLink from subdir")
	assert.Equal(t, want, got, "FindLink = %+v, want %+v", got, want)

	require.NoError(t, RemoveLink(), "RemoveLink")
	_, _, err = FindLink()
	assert.ErrorIs(t, err, os.ErrNotExist, "FindLink after remove = %v, want ErrNotExist", err)
	assert.ErrorIs(t, RemoveLink(), os.ErrNotExist, "RemoveLink when absent, want ErrNotExist")
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"localhost:3000":   "https://localhost:3000",
		"http://x/":        "http://x",
		"https://y":        "https://y",
		"  https://z/  ":   "https://z",
		"example.com/api/": "https://example.com/api",
	}
	for in, want := range cases {
		assert.Equal(t, want, normalizeURL(in), "normalizeURL(%q)", in)
	}
}
