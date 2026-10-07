package promote

import (
	"io"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Removing a tile sweeps the share volumes no remaining tile row names, and
// only those: a volume a live row names (a deploy's, still pulling its
// image, held by no container) survives.
func TestRemoveSweepsFromRows(t *testing.T) {
	w := setup(t)
	d := w.f.D
	vol := d.Volumes.WithShares(w.s.Shares)
	s, err := vol.CreateShare(ctx, w.st.OrgID, volume.ShareSpec{Slug: "media", Kind: volume.NFS, Source: "nas:/e"})
	must(t, err)
	mk := func(slug, line string) store.Tile {
		tl, err := d.Tiles.Create(ctx, store.Tile{StackID: w.st.ID, EnvironmentID: w.dev.ID, Name: slug, Slug: slug,
			Kind: tile.Service, GitURL: "https://github.com/acme/api", Volumes: line})
		must(t, err)
		return tl
	}
	mk("keeper", "share:media/a:/d")
	gone := mk("goner", "share:media/b:/d")
	lbl := map[string]string{volume.ShareLabel: s.ID}
	live, dead := volume.ShareVolumeName(s, "a"), volume.ShareVolumeName(s, "b")
	w.fake.Volumes = []docker.VolumeInfo{{Name: live, Labels: lbl}, {Name: dead, Labels: lbl}}
	_, err = w.f.Remove(ctx, w.dev, []store.Tile{gone}, io.Discard)
	must(t, err)
	var removed []string
	for _, c := range w.fake.Calls() {
		if c.Method == "RemoveVolume" {
			removed = append(removed, c.Args[0])
		}
	}
	if len(removed) != 1 || removed[0] != dead {
		t.Errorf("removed %v, want only %s", removed, dead)
	}
}
