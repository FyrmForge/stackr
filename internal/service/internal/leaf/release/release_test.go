package release_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReleases(t *testing.T) {
	st := servicetest.Store(t)
	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        org,
		Name:      "o",
		Slug:      "o",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        stack,
		OrgID:     org,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		Domains:   "[]",
		CreatedAt: now,
	}))
	img1, img2 := "i1", "i2"
	for _, id := range []string{img1, img2} {
		must(t, st.Images.Create(ctx, store.Image{
			ID:        id,
			Ref:       "stkr/api:" + id,
			Digest:    "sha256:" + id,
			CreatedAt: now,
		}))
	}
	l := release.New(st.Releases, st.ReleaseTiles)

	if _, err := l.Create(ctx, stack, "push", []release.Pin{{Slug: "db", Digest: "16"}}); err == nil {
		t.Error("a tag accepted as a digest")
	}
	if _, err := l.Create(ctx, stack, "push", []release.Pin{{Slug: "a"}, {Slug: "a"}}); err == nil {
		t.Error("duplicate slug accepted")
	}
	r1, err := l.Create(ctx, stack, "push", []release.Pin{
		{
			Slug:      "config",
			Repo:      "acme/stack",
			Branch:    "main",
			CommitSHA: "c1",
		},
		{
			Slug:      "api",
			Repo:      "acme/api",
			Branch:    "main",
			CommitSHA: "a1",
			ImageID:   &img1,
		},
		{
			Slug:   "db",
			Digest: "sha256:pg16",
		},
	})
	must(t, err)
	// A push to the api repo rebuilds api only; everything else is copied.
	r2, err := l.Derive(ctx, stack, r1.ID, "push", release.Pin{
		Slug:      "api",
		Repo:      "acme/api",
		Branch:    "main",
		CommitSHA: "a2",
		ImageID:   &img2,
	})
	must(t, err)
	if r1.Number != 1 || r2.Number != 2 {
		t.Errorf("numbers = %d, %d", r1.Number, r2.Number)
	}
	p1, _ := l.Pins(ctx, r1.ID)
	p2, _ := l.Pins(ctx, r2.ID)
	if got := release.Diff(p1, p2); !reflect.DeepEqual(got, []release.Change{{Slug: "api", Kind: "changed"}}) {
		t.Errorf("diff = %v", got)
	}
	delete(p2, "db")
	p2["web"] = release.Pin{Slug: "web", Digest: "sha256:w"}
	if got := release.Diff(p1, p2); !reflect.DeepEqual(got, []release.Change{
		{"api", "changed"},
		{"db", "removed"},
		{"web", "added"},
	}) {
		t.Errorf("diff = %v", got)
	}
	if list, _ := l.List(ctx, stack); len(list) != 2 || list[0].ID != r2.ID {
		t.Errorf("list = %v", list)
	}
	ids, _ := l.ImageIDs(ctx)
	if len(ids) != 2 {
		t.Errorf("image ids = %v", ids)
	}
	got, _ := l.GetByNumber(ctx, stack, 2)
	if got.ID != r2.ID || got.CreatedBy != "push" {
		t.Errorf("by number = %+v", got)
	}
}
