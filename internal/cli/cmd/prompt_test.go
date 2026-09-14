package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// promptRuntime is a Runtime whose stdin pretends to be a TTY and whose huh
// forms run in accessible mode, so prompts read plain lines, machine-testable
// without a pty.
func promptRuntime(input string) (*Runtime, *bytes.Buffer) {
	var errb bytes.Buffer
	rt := &Runtime{
		Stdin:    strings.NewReader(input),
		Stdout:   &bytes.Buffer{},
		Stderr:   &errb,
		StdinTTY: true,
		Getenv: func(k string) string {
			if k == "STACKR_ACCESSIBLE" {
				return "1"
			}
			return ""
		},
	}
	return rt, &errb
}

func TestConfirmAcceptAccessible(t *testing.T) {
	rt, _ := promptRuntime("y\n")
	require.NoError(t, rt.Confirm("Remove thing X?"))
}

func TestConfirmRejectAccessible(t *testing.T) {
	rt, _ := promptRuntime("n\n")
	require.ErrorIs(t, rt.Confirm("Remove thing X?"), ErrCancelled)
}

func TestConfirmEOF(t *testing.T) {
	rt, _ := promptRuntime("")
	require.Error(t, rt.Confirm("Remove thing X?"), "EOF stdin must not read as consent")
}

func TestConfirmTitleShown(t *testing.T) {
	rt, errb := promptRuntime("y\n")
	_ = rt.Confirm("Remove slice acme:db (main)?")
	assert.Contains(t, errb.String(), "Remove slice acme:db (main)?")
}

func TestConfirmYesSkipsPrompt(t *testing.T) {
	rt, errb := promptRuntime("") // no input available, must not be read
	rt.Yes = true
	require.NoError(t, rt.Confirm("Remove thing X?"))
	assert.Zero(t, errb.Len(), "--yes must not render a prompt")
}

func TestPromptSecretAccessible(t *testing.T) {
	rt, errb := promptRuntime("sk_hunter2\n")
	got, err := rt.PromptSecret("API key: ")
	require.NoError(t, err)
	assert.Equal(t, "sk_hunter2", got)
	assert.NotContains(t, errb.String(), "sk_hunter2", "secret must not echo")
}

func TestPromptSecretPipedNonTTY(t *testing.T) {
	rt, _ := promptRuntime("sk_piped\n")
	rt.StdinTTY = false
	got, err := rt.PromptSecret("API key: ")
	require.NoError(t, err)
	assert.Equal(t, "sk_piped", got)
}

func TestPromptSecretWithSpaces(t *testing.T) {
	rt, _ := promptRuntime("my pass word\n")
	rt.StdinTTY = false
	got, err := rt.PromptSecret("Password: ")
	require.NoError(t, err)
	assert.Equal(t, "my pass word", got, "the whole line is the secret")
}

func TestPromptSecretNoTrailingNewline(t *testing.T) {
	rt, _ := promptRuntime("bare")
	rt.StdinTTY = false
	got, err := rt.PromptSecret("Password: ")
	require.NoError(t, err)
	assert.Equal(t, "bare", got)
}

func TestChooseAccessible(t *testing.T) {
	names := []string{"alpha", "beta", "gamma"}
	rt, _ := promptRuntime("2\n") // accessible select takes a number
	i, err := rt.choose("stack", "--stack", "", len(names), func(i int) (string, string) {
		return names[i], names[i] + "-id"
	})
	require.NoError(t, err)
	assert.Equal(t, 1, i, "want beta")
}

func TestChooseByFlagAndAuto(t *testing.T) {
	rt, _ := promptRuntime("")
	// id flag match needs no input
	i, err := rt.choose("stack", "--stack", "b-id", 3, func(i int) (string, string) {
		return []string{"a", "b", "c"}[i], []string{"a-id", "b-id", "c-id"}[i]
	})
	require.NoError(t, err)
	assert.Equal(t, 1, i)
	// a lone option is auto-picked silently
	i, err = rt.choose("app", "--app", "", 1, func(int) (string, string) { return "only", "only-id" })
	require.NoError(t, err)
	assert.Equal(t, 0, i)
	// non-TTY with several options refuses, naming the flag
	rt.StdinTTY = false
	_, err = rt.choose("stack", "--stack", "", 2, func(i int) (string, string) { return "x", "x" })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--stack")
}
