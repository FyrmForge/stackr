package netaddr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTrusted(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.100":    "192.168.1.100/32",
		"192.168.1.100/32": "192.168.1.100/32",
		"192.168.1.7/24":   "192.168.1.0/24",
		"2001:db8::1":      "2001:db8::1/128",
		"2001:db8::/32":    "2001:db8::/32",
	} {
		got, err := ParseTrusted(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "nope", "999.1.1.1", "10.0.0.0/33", "0.0.0.0/0", "::/0", "192.168.1.100/"} {
		_, err := ParseTrusted(bad)
		assert.Error(t, err, bad)
	}
}
