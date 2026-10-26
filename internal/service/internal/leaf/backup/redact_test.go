package backup

import "testing"

func TestRedact(t *testing.T) {
	for _, c := range []struct{ msg, key, want string }{
		{"context deadline exceeded", "x", "context deadline exceeded"},
		{"bad key x: denied", "x", "bad key ***: denied"},
		{"Credential=AKIAEXAMPLE1/2026/us-east-1", "AKIAEXAMPLE1", "Credential=***/2026/us-east-1"},
		{"sig%2FAKIAEXAMPLE1%2B", "AKIAEXAMPLE1", "sig%2F***%2B"}, // a long key goes anywhere
		{"no key here", "", "no key here"},
	} {
		if got := redact(c.msg, c.key); got != c.want {
			t.Errorf("redact(%q, %q) = %q, want %q", c.msg, c.key, got, c.want)
		}
	}
}
