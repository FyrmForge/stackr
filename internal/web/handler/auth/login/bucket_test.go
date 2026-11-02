package login

import "testing"

// An IPv6 client counts as its /64; an IPv4 one as itself.
func TestBucket(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":                "203.0.113.9",
		"2001:db8:1:2:aaaa::1":       "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb:cccc::ff": "2001:db8:1:2::/64",
		"::ffff:203.0.113.9":         "::ffff:203.0.113.9",
		"not-an-ip":                  "not-an-ip",
	} {
		if got := bucket(in); got != want {
			t.Errorf("bucket(%q) = %q, want %q", in, got, want)
		}
	}
}
