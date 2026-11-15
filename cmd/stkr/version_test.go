package main

import "testing"

func TestShownVersion(t *testing.T) {
	for _, c := range []struct{ stamped, module, want string }{
		{"v0.8.24", "(devel)", "v0.8.24"},
		{"abc1234", "v0.8.24", "abc1234"},
		{"dev", "v0.8.24", "v0.8.24"},
		{"dev", "", "dev"},
		{"dev", "(devel)", "dev"},
		{"dev", "v0.8.24+dirty", "dev"},
		{"dev", "v0.8.25-0.20261115120000-0123456789ab", "dev"},
		{"dev", "v0.8.25-0.20261115120000-0123456789ab+dirty", "dev"},
	} {
		if got := shownVersion(c.stamped, c.module); got != c.want {
			t.Errorf("shownVersion(%q, %q) = %q, want %q", c.stamped, c.module, got, c.want)
		}
	}
}
