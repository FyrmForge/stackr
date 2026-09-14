package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chdir into a temp dir holding .stackr/link.json with the given body.
func linked(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, linkDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, linkDir, linkFile), []byte(body), 0o644))
	t.Chdir(dir)
}

// Link files written before "project" was renamed to "stack" must keep working:
// a plain unmarshal leaves Stack empty, which every caller reads as "this
// directory was never linked", silently, with no error to explain it.
func TestFindLinkReadsPreRenameFile(t *testing.T) {
	linked(t, `{"project":"stk_1","project_name":"Shop","env":"env_1","app":"app_1"}`)

	l, _, err := FindLink()
	require.NoError(t, err)
	assert.Equal(t, "stk_1", l.Stack)
	assert.Equal(t, "Shop", l.StackName)
	assert.Equal(t, "env_1", l.Env)
	assert.Equal(t, "app_1", l.App)
}

func TestFindLinkPrefersCurrentKeys(t *testing.T) {
	// Both present: the current key wins, so a re-linked directory is not
	// dragged back to a stale id.
	linked(t, `{"stack":"new","stack_name":"New","project":"old","project_name":"Old"}`)

	l, _, err := FindLink()
	require.NoError(t, err)
	assert.Equal(t, "new", l.Stack)
	assert.Equal(t, "New", l.StackName)
}

// SaveLink writes only the current keys, so a rewritten file has no old ones.
func TestSaveLinkWritesCurrentKeys(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveLink(dir, Link{Stack: "stk_2", StackName: "Shop", Env: "env_2"}))

	b, err := os.ReadFile(filepath.Join(dir, linkDir, linkFile))
	require.NoError(t, err)
	assert.Contains(t, string(b), `"stack": "stk_2"`)
	assert.NotContains(t, string(b), `"project"`)
}
