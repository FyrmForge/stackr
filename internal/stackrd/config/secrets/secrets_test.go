package secrets

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip(t *testing.T) {
	require.NoError(t, Load(filepath.Join(t.TempDir(), "master.key")))
	enc := Encrypt("KEY=value\nMULTI=\"a\nb\"")
	require.True(t, strings.HasPrefix(enc, "enc:"), "not encrypted: %q", enc)
	require.Equal(t, enc, Encrypt(enc), "double encryption")
	dec, err := Decrypt(enc)
	require.NoError(t, err)
	require.Equal(t, "KEY=value\nMULTI=\"a\nb\"", dec)
	pt, _ := Decrypt("plain KEY=V")
	require.Equal(t, "plain KEY=V", pt, "plaintext passthrough broken")
	require.Equal(t, "", Encrypt(""), "empty should pass through")
}

// Every credential stackr mints for itself comes from here, so "it is random,
// the right length, and not shared between calls" is worth one check.
func TestRandomHex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s := RandomHex(16)
		require.Len(t, s, 32, "RandomHex(16) = %q, want 32 hex chars", s)
		require.Equal(t, "", strings.Trim(s, "0123456789abcdef"), "not hex: %q", s)
		require.False(t, seen[s], "repeated value after %d calls: %q", i, s)
		seen[s] = true
	}
	assert.Len(t, RandomHex(24), 48, "length must follow the byte count asked for")
}
