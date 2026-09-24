# Extract: `internal/netaddr`

Source: `netaddr/netaddr.go`, `netaddr/netaddr_test.go` (base `internal/`)
Commit: c2423f0
Taken: as is — the whole package and its test
Cut: —
Cuts belong to: —

The row says "as is", and the package already obeys the layering rules it would
be filtered by: no store, no Docker, no auth, standard library only. It comes
over unchanged, same path (`internal/netaddr`), so the installer binary can
keep sharing it with the service.

## `netaddr/netaddr.go`

```go
// Package netaddr checks addresses typed by an operator. It imports nothing
// from the panel so the installer binary can share it.
package netaddr

import (
	"fmt"
	"net"
	"strings"
)

// ParseTrusted normalises one trusted proxy entry: a bare address becomes a
// single-host range (/32, /128), a range is written in its canonical form. A
// /0 is refused, it would trust every address on the internet to name the
// client.
func ParseTrusted(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		ip := net.ParseIP(s)
		if ip == nil {
			return "", fmt.Errorf("%q is not an IP address or range", s)
		}
		if ip.To4() != nil {
			return ip.String() + "/32", nil
		}
		return ip.String() + "/128", nil
	}
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return "", fmt.Errorf("%q is not an IP address or range", s)
	}
	if ones, _ := n.Mask.Size(); ones == 0 {
		return "", fmt.Errorf("%q trusts every address; name your proxy's address instead", s)
	}
	return n.String(), nil
}
```

## `netaddr/netaddr_test.go`

```go
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
```

The bad-input list is the spec, not filler: empty string, garbage, an octet out
of range, a prefix length out of range, both `/0` forms, and a trailing slash
with nothing after it.

## Notes for the builder

- **Canonicalisation is the point, not validation.** `192.168.1.7/24` comes back
  as `192.168.1.0/24`: the host bits are masked off by `net.ParseCIDR`, so two
  operators typing different addresses in the same subnet produce one stored
  value. Anything comparing trusted-proxy entries can compare strings.
- **The `/0` refusal is a security rule.** Trusting `0.0.0.0/0` means any client
  can set `X-Forwarded-For` and name itself; the error text tells the operator
  what to type instead. Do not relax it to a warning.
- **`strings.TrimSpace` on entry** is what makes a pasted or comma-split list
  work without every caller trimming. Keep it if the config parser feeds this
  directly.
- **Only dependency is testify** (test file only). If the new repo does not
  carry `stretchr/testify`, the test is a five-minute stdlib rewrite — two
  loops, `t.Errorf` — and nothing in the package itself needs it.

Size: source 61 lines, extract 107 lines (whole `.md`; kept Go alone is 61 —
this row is "as is", so the code count equals the source exactly and the
difference is the format's header and notes)
