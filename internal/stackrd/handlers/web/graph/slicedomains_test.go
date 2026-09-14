package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The bug this guards: every slice used to inherit its instance's domains, so
// a private bucket advertised a public hostname and a postgres logical db
// showed one for a protocol Traefik cannot route at all.
func TestSliceDomains(t *testing.T) {
	hosts := []string{"cdn.example.com"}
	cases := []struct {
		name   string
		kind   string
		public bool
		want   int
	}{
		{"public bucket keeps the host", "s3", true, 1},
		{"private bucket shows nothing", "s3", false, 0},
		{"postgres slice never shows a host", "postgres", false, 0},
		{"postgres marked public still shows nothing", "postgres", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Len(t, SliceDomains(c.kind, c.public, hosts), c.want)
		})
	}
}
