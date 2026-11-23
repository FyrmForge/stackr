package image

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// An image whose state cannot be read says "unknown": never blank, never
// "up to date" on a guess.
func TestStatus(t *testing.T) {
	for _, c := range []struct {
		img  store.Image
		want string
	}{
		{store.Image{}, "unknown"},
		{store.Image{Digest: "a"}, "unknown"},
		{store.Image{Digest: "a", LastDigest: "a", LastError: "429"}, "unknown"},
		{store.Image{LastDigest: "b"}, "unknown"},
		{store.Image{Digest: "a", LastDigest: "a"}, "up to date"},
		{store.Image{Digest: "a", LastDigest: "b"}, "newer image available"},
	} {
		if got := Status(c.img); got != c.want {
			t.Errorf("%+v = %q, want %q", c.img, got, c.want)
		}
	}
}
