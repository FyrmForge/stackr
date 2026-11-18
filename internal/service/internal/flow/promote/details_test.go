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
	)
	want := "Engine: postgres\nMajor: 18\nImage: postgres:18.6\nVolumes: pgdata:/var/lib/postgresql, bk:/b"
	if got != want {
		t.Errorf("details = %q, want %q", got, want)
	}
	if got := createDetails(TileConf{}, store.Tile{}); got != "" {
		t.Errorf("an empty block has details %q", got)
	}
}
