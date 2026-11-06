package service

import "testing"

func TestHostsOverlap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"a.io", "a.io", true},
		{"*.a.io", "x.a.io", true},
		{"x.a.io", "*.a.io", true},
		{"*.a.io", "a.io", false},
		{"x.a.io", "y.a.io", false},
		{"*.a.io", "x.y.a.io", false},
		{"*.a.io", "*.b.io", false},
	} {
		if got := hostsOverlap(c.a, c.b); got != c.want {
			t.Errorf("hostsOverlap(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
