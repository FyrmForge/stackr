package service

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.1.1", "v0.1.0", true},
		{"v0.2.0", "v0.1.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.1", false},
		{"v0.1.1", "dev", false},
		{"0.1.1", "v0.1.0", false},
		{"", "v0.1.0", false},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if got := imageRef(panelRepo, "v0.1.1"); got != "ghcr.io/fyrmforge/stackr:0.1.1" {
		t.Errorf("imageRef = %s", got)
	}
}
