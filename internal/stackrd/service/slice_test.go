package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type sliceStore struct {
	repo.Store
	p *repo.Provision
}

func (s sliceStore) GetProvision(context.Context, string) (*repo.Provision, error) { return s.p, nil }

// The API took the variable name raw where the panel folded it, so a name
// with a space or a dash in it produced a container env line no shell reads.
func TestEnvVarName(t *testing.T) {
	for in, want := range map[string]string{
		"DATABASE_URL": "DATABASE_URL",
		"db url":       "DBURL",
		"pg-url":       "PGURL",
		"9lives":       "LIVES", // a leading digit is not a legal identifier
		"  db  ":       "DB",
		"!!!":          "",
	} {
		if got := EnvVarName(in); got != want {
			t.Errorf("EnvVarName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A managed instance consuming another instance's slice is a database inside
// a database, and a volume has no env to inject into.
func TestOnlyServicesAndCronsConsumeSlices(t *testing.T) {
	for _, consumer := range []*repo.Tile{
		{ID: "d1", Engine: "postgres", Kind: "service"}, // managed
		{ID: "v1", Kind: "volume"},
	} {
		if err := consumable(consumer); err == nil {
			t.Errorf("%s should not be able to hold a slice", consumer.ID)
		}
	}
	if err := consumable(&repo.Tile{ID: "a1", Kind: "service"}); err != nil {
		t.Errorf("a plain service must be able to hold a slice: %v", err)
	}
}

// Ownership, not just existence: a provision id belonging to another tile
// reads as missing rather than as someone else's row.
func TestDetachChecksOwnership(t *testing.T) {
	st := sliceStore{p: &repo.Provision{ID: "p1", ConsumerTileID: "other"}}
	svc := NewSliceService(st, nil, nil, nil)
	err := svc.Detach(context.Background(), &repo.Tile{ID: "mine"}, "p1")
	if err != svcerr.ErrNotFound {
		t.Fatalf("want not found, got %v", err)
	}
}
