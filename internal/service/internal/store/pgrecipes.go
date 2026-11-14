package store

import (
	"context"
	"time"
)

// PGRecipe is a row of pg_recipes: an admin-added Postgres extension recipe,
// or a bare row with Enabled false that switches a built-in off. ID holds the
// recipe's name. Spec is the recipe definition as JSON (flow/operated.Recipe);
// Share is "all" or a list of org ids.
type PGRecipe struct {
	ID        string    `db:"id" json:"id"`
	Spec      string    `db:"spec" json:"spec"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	Share     NameList  `db:"share" json:"share"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type PGRecipeStore interface {
	List(ctx context.Context) ([]PGRecipe, error)
	Get(ctx context.Context, id string) (PGRecipe, error)
	// Upsert inserts the row or rewrites spec, enabled and share of the one
	// with its id; created_at of an existing row stays.
	Upsert(ctx context.Context, r PGRecipe) error
	Delete(ctx context.Context, id string) error
}

var pgRecipesT = newTable[PGRecipe]("pg_recipes", nil)

type pgRecipes struct{ crud[PGRecipe] }

func (s pgRecipes) List(ctx context.Context) ([]PGRecipe, error) {
	return s.many(ctx, "1 = 1 ORDER BY id")
}

func (s pgRecipes) Upsert(ctx context.Context, r PGRecipe) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.q.NamedExecContext(ctx, s.t.insert+
		" ON CONFLICT (id) DO UPDATE SET spec = excluded.spec, enabled = excluded.enabled, share = excluded.share", r)
	return mapErr(err)
}
