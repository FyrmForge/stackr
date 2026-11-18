package promote

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A create row says what the new tile will be; empty lines are left out.
func TestCreateDetails(t *testing.T) {
	got := createDetails(
		TileConf{Engine: "postgres", Major: 18, Volumes: []string{"pgdata:/var/lib/postgresql", "bk:/b"}},
		store.Tile{ImageRef: "postgres:18.6"},
		"vm-hel-2",
		nil,
	)
	want := "Engine: postgres\nMajor: 18\nImage: postgres:18.6\nNode: vm-hel-2\nVolumes: pgdata:/var/lib/postgresql, bk:/b"
	if got != want {
		t.Errorf("details = %q, want %q", got, want)
	}
	if got := createDetails(TileConf{}, store.Tile{}, "", nil); got != "" {
		t.Errorf("an empty block has details %q", got)
	}
}

// A create row's volumes read as names, new ones flagged, with the file's cap.
func TestCreateDetailsVolumes(t *testing.T) {
	word := func(spec string) string { return "pgdata, new, 5120 MB" }
	got := createDetails(TileConf{Volumes: []string{"pgdata:/var/lib/postgresql"}}, store.Tile{}, "", word)
	if got != "Volumes: pgdata, new, 5120 MB" {
		t.Errorf("details = %q", got)
	}
}
