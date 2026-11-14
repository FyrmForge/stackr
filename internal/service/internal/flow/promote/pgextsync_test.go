package promote

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// An env sync carries the instance's extension list: an instance whose list
// differs is an Edited tile, and the list goes through the PGExt hook. Two
// instances with no extensions stay untouched.
func TestSyncCarriesExtensions(t *testing.T) {
	w := syncWorld(t)
	var got [][]string
	w.f.PGExt = func(_ context.Context, _ store.Tile, _ store.OperatedInstance, _ *string, _, exts *[]string, _ io.Writer) error {
		if exts != nil {
			got = append(got, *exts)
		}
		return nil
	}
	dev := w.mk(t, w.dev, "db", store.Tile{Kind: tile.Operated, ImageRef: "postgres:17.5"})
	dm, err := w.f.D.Operated.Create(ctx, dev.ID, "postgres", "stackr", "")
	must(t, err)
	prd := w.mk(t, w.prd, "db", store.Tile{Kind: tile.Operated, ImageRef: "postgres:17.5"})
	_, err = w.f.D.Operated.Create(ctx, prd.ID, "postgres", "stackr", "")
	must(t, err)
	if s := w.syncPlan(t); len(s.Changes) != 0 || len(s.Tiles) != 0 {
		t.Fatalf("no extensions on either side moved: %v %s", s.Changes, tags(s))
	}
	must(t, w.s.OperatedInstances.SetBuild(ctx, dm.ID, nil, []string{"pgcrypto"}, "{}"))
	s := w.syncPlan(t)
	if tags(s) != "db:edited" || !strings.Contains(kinds(&s.Plan), "operated:dbextensions+pgcrypto") || s.Blocked() {
		t.Fatalf("plan: %s %s %v", tags(s), kinds(&s.Plan), s.Blockers)
	}
	_, err = w.syncApply(t, s, "db")
	must(t, err)
	if len(got) != 1 || !slices.Equal(got[0], []string{"pgcrypto"}) {
		t.Errorf("PGExt calls = %v", got)
	}
}

// A new instance synced from a recipe-built one does not take the source's
// built image as a custom image: it starts on the official base and builds for
// the list it carries, before its first rollout. The derived startup modules
// are not copied as hand-typed ones.
func TestSyncNewInstanceFromBuiltSource(t *testing.T) {
	w := syncWorld(t)
	built := "localhost:5000/acme/shop/db:17.5-ext-amd64-aaaa"
	dev := w.mk(t, w.dev, "db", store.Tile{Kind: tile.Operated, ImageRef: built})
	dm, err := w.f.D.Operated.Create(ctx, dev.ID, "postgres", "stackr", "")
	must(t, err)
	must(t, w.s.OperatedInstances.SetBuild(ctx, dm.ID, []string{"vchord"}, []string{"vector"},
		`{"base":"postgres:17.5","recipes":["pgvector"],"ref":"`+built+`"}`))
	var seen []store.Tile
	var applied [][]string
	var plain []pgExtCall
	w.f.PGExtPlan = func(_ context.Context, tl store.Tile, _ store.OperatedInstance, exts []string, _ io.Writer) ([]string, bool, error) {
		seen = append(seen, tl)
		return []string{"builds an image"}, true, nil
	}
	w.f.PGExtApply = func(_ context.Context, _ store.Tile, exts []string, _ io.Writer) error {
		applied = append(applied, exts)
		return nil
	}
	w.f.PGExt = func(_ context.Context, tl store.Tile, _ store.OperatedInstance, image *string, preload, exts *[]string, _ io.Writer) error {
		plain = append(plain, pgExtCall{tl.Slug, image, preload, exts})
		return nil
	}
	s := w.syncPlan(t)
	if tags(s) != "db:new" || s.Blocked() {
		t.Fatalf("plan: %s %v", tags(s), s.Blockers)
	}
	if len(seen) != 1 || seen[0].ID != "" || seen[0].ImageRef != "postgres:17.5" {
		t.Fatalf("plan hook tiles = %+v", seen)
	}
	// the fake docker cannot roll an instance out; the hooks have run by then
	if _, err := w.syncApply(t, s, "db"); err != nil && !strings.Contains(err.Error(), "deploy ") {
		t.Fatal(err)
	}
	if got := w.tileIn(t, w.prd, "db"); got.ImageRef != "postgres:17.5" {
		t.Errorf("the new instance runs %q, want the official base", got.ImageRef)
	}
	if len(applied) != 1 || !slices.Equal(applied[0], []string{"vector"}) {
		t.Errorf("build applies = %v", applied)
	}
	for _, c := range plain {
		if c.preload != nil || c.exts != nil {
			t.Errorf("the plain path was handed %+v", c)
		}
	}
}
