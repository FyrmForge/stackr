package stack_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{
		ID:        id,
		Name:      id,
		Slug:      id[:8],
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRoundTrip(t *testing.T) {
	st := servicetest.Store(t)
	l := stack.New(st.Stacks)
	org, other := seedOrg(t, st), seedOrg(t, st)

	s, err := l.Create(ctx, org, "My Shop!", "d")
	if err != nil || s.Slug != "my-shop" {
		t.Fatalf("create = %+v, %v", s, err)
	}
	for _, bad := range []string{"", "!!", "params", "my shop"} {
		if _, err := l.Create(ctx, org, bad, ""); err == nil {
			t.Errorf("create %q accepted", bad)
		}
	}
	if _, err := l.Create(ctx, other, "my shop", ""); err != nil {
		t.Errorf("same slug in another org: %v", err)
	}

	b, _ := l.Create(ctx, org, "b", "")
	if _, err := l.Rename(ctx, b, "My-Shop"); err == nil {
		t.Error("rename onto a taken slug accepted")
	}
	if s, err = l.Rename(ctx, s, "My Shop"); err != nil {
		t.Errorf("rename onto own slug: %v", err)
	}
	if s, err = l.Rename(ctx, s, "Store"); err != nil || s.Slug != "store" {
		t.Fatalf("rename = %+v, %v", s, err)
	}

	if s, err = l.SetConfigRepo(ctx, s, "c1", "acme/infra", "main", "stackr.yml"); err != nil {
		t.Fatal(err)
	}
	if s, err = l.SetSettings(ctx, s, `{"cpu":1}`); err != nil {
		t.Fatal(err)
	}

	got, err := l.GetBySlug(ctx, org, "store")
	if err != nil || got.ConfigRepo != "https://github.com/acme/infra" || got.ConfigPath != "stackr.yml" || got.Settings != `{"cpu":1}` {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// Both spellings store the URL the connector lookup reads the host off.
	for _, repo := range []string{
		" acme/infra ",
		"https://github.com/acme/infra",
	} {
		if got, err = l.SetConfigRepo(ctx, got, "c1", repo, "main", "stackr.yml"); err != nil || got.ConfigRepo != "https://github.com/acme/infra" {
			t.Errorf("SetConfigRepo(%q) stored %q, %v", repo, got.ConfigRepo, err)
		}
	}
	for _, repo := range []string{
		"git@github.com:acme/infra.git",
		"https://gitlab.example.com/acme/infra",
		"gitlab.example.com/acme/infra",
	} {
		if got, err = l.SetConfigRepo(ctx, got, "c1", repo, "main", "stackr.yml"); err != nil || got.ConfigRepo != repo {
			t.Errorf("SetConfigRepo(%q) stored %q, %v; want it as typed", repo, got.ConfigRepo, err)
		}
	}

	if got, _ = l.SetConfigRepo(ctx, got, "c1", "", "main", "x"); got.ConfigBranch != "" {
		t.Errorf("clearing the repo kept %+v", got)
	}
	if err := l.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Get(ctx, s.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("get after delete = %v", err)
	}
}
