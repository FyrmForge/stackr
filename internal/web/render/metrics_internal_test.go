package render

import "testing"

// The tunnel's events URL already has a query; after= joins it, it never
// starts a second one (that was a 404 and no live tunnel graphs).
func TestWithAfter(t *testing.T) {
	for in, want := range map[string]string{
		"/-/tiles/web/metrics/events":      "/-/tiles/web/metrics/events?after=7",
		"/-/cluster/tunnel/events?a=x&b=y": "/-/cluster/tunnel/events?a=x&b=y&after=7",
	} {
		if got := withAfter(in, 7); got != want {
			t.Errorf("withAfter(%q) = %q, want %q", in, got, want)
		}
	}
}
