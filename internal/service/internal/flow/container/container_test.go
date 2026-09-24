package container

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error { return nil }
func (vipStub) Remove(context.Context, string) error        { return nil }

func TestTileVerbsAndGuard(t *testing.T) {
	ctx := context.Background()
	fake := dockerfake.New()
	lbl := map[string]string{tile.LabelTile: "t1", tile.LabelRole: "replica"}
	fake.Containers = []docker.Container{{ID: "a", Name: "api-1", Labels: lbl}, {ID: "b", Name: "api-2", Labels: lbl},
		{ID: "panel", Name: "stackr", Labels: map[string]string{tile.LabelSystem: "true"}}}
	f := &Flow{Tiles: tile.New(servicetest.Store(t).Tiles, fake, vipStub{})}
	api := store.Tile{ID: "t1", Slug: "api"}
	if err := f.Restart(ctx, api, io.Discard); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range fake.Calls() {
		if c.Method == "Restart" {
			got = append(got, c.Args[0])
		}
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("restarted %v", got)
	}
	if err := f.Tiles.StartContainer(ctx, "", "panel"); err == nil {
		t.Error("the panel container was started from here")
	} else if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("guard error = %v", err)
	}
	if _, _, err := f.Tiles.Terminal(ctx, "", "panel", []string{"sh"}, nil); err == nil {
		t.Error("a terminal opened into the panel")
	}
	if err := f.Start(ctx, store.Tile{ID: "none", Slug: "x"}, io.Discard); err == nil {
		t.Error("start of a tile with no containers passed")
	}
}
