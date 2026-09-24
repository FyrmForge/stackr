package store

import (
	"context"
	"time"
)

// Release is a row of releases.
type Release struct {
	ID        string    `db:"id"`
	StackID   string    `db:"stack_id"`
	Number    int       `db:"number"`
	CreatedAt time.Time `db:"created_at"`
	CreatedBy string    `db:"created_by"`
}

type ReleaseStore interface {
	Create(ctx context.Context, r Release) error
	Get(ctx context.Context, id string) (Release, error)
	GetByNumber(ctx context.Context, stackID string, number int) (Release, error)
	ListByStack(ctx context.Context, stackID string) ([]Release, error)
	Update(ctx context.Context, r Release) error
	Delete(ctx context.Context, id string) error
}

var releasesT = newTable[Release]("releases", nil)

type releases struct{ crud[Release] }

func (s releases) GetByNumber(ctx context.Context, stackID string, number int) (Release, error) {
	return s.one(ctx, "stack_id = ? AND number = ?", stackID, number)
}

func (s releases) ListByStack(ctx context.Context, stackID string) ([]Release, error) {
	return s.many(ctx, "stack_id = ?", stackID)
}

// ReleaseTile is a row of release_tiles: one tile's pinned source and image.
type ReleaseTile struct {
	ID        string  `db:"id"`
	ReleaseID string  `db:"release_id"`
	Slug      string  `db:"slug"`
	Repo      string  `db:"repo"`
	Branch    string  `db:"branch"`
	CommitSHA string  `db:"commit_sha"`
	ImageID   *string `db:"image_id"`
	Digest    string  `db:"digest"`
}

type ReleaseTileStore interface {
	Create(ctx context.Context, r ReleaseTile) error
	Get(ctx context.Context, id string) (ReleaseTile, error)
	ListByRelease(ctx context.Context, releaseID string) ([]ReleaseTile, error)
	// ImageIDs is every image any release pins: image cleanup keeps them.
	ImageIDs(ctx context.Context) ([]string, error)
	Update(ctx context.Context, r ReleaseTile) error
	Delete(ctx context.Context, id string) error
}

var releaseTilesT = newTable[ReleaseTile]("release_tiles", nil)

type releaseTiles struct{ crud[ReleaseTile] }

func (s releaseTiles) ListByRelease(ctx context.Context, releaseID string) ([]ReleaseTile, error) {
	return s.many(ctx, "release_id = ?", releaseID)
}

func (s releaseTiles) ImageIDs(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.q.SelectContext(ctx, &ids, "SELECT DISTINCT image_id FROM release_tiles WHERE image_id IS NOT NULL")
	return ids, mapErr(err)
}
