package store

import (
	"context"
	"strings"
	"time"
)

// Release is a row of releases.
type Release struct {
	ID        string    `db:"id" json:"id"`
	StackID   string    `db:"stack_id" json:"stack_id"`
	Number    int       `db:"number" json:"number"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	CreatedBy string    `db:"created_by" json:"created_by"`
	// Commit is the commit its pins were cut from ("" = none); filled by the
	// release leaf on List/Get, never stored here.
	Commit string `db:"-" json:"commit,omitempty"`
	// Message is the first line of that commit's message; "" when the cut had
	// none (apply, image watch, manual deploy).
	Message string `db:"message" json:"message,omitempty"`
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
	ID        string  `db:"id" json:"id"`
	ReleaseID string  `db:"release_id" json:"release_id"`
	Slug      string  `db:"slug" json:"slug"`
	Repo      string  `db:"repo" json:"repo"`
	Branch    string  `db:"branch" json:"branch"`
	CommitSHA string  `db:"commit_sha" json:"commit_sha"`
	ImageID   *string `db:"image_id" json:"image_id"`
	Digest    string  `db:"digest" json:"digest"`
}

type ReleaseTileStore interface {
	Create(ctx context.Context, r ReleaseTile) error
	Get(ctx context.Context, id string) (ReleaseTile, error)
	ListByRelease(ctx context.Context, releaseID string) ([]ReleaseTile, error)
	// ConfigCommits is the config pin's commit per release id, one query;
	// a release without a config pin is absent.
	ConfigCommits(ctx context.Context, releaseIDs []string) (map[string]string, error)
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

func (s releaseTiles) ConfigCommits(ctx context.Context, releaseIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(releaseIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(releaseIDs))
	for i, id := range releaseIDs {
		args[i] = id
	}
	var rows []struct {
		ReleaseID string `db:"release_id"`
		CommitSHA string `db:"commit_sha"`
	}
	err := s.q.SelectContext(ctx, &rows,
		"SELECT release_id, commit_sha FROM release_tiles WHERE slug = '_config' AND commit_sha != '' AND release_id IN (?"+strings.Repeat(", ?", len(releaseIDs)-1)+")", args...)
	for _, r := range rows {
		out[r.ReleaseID] = r.CommitSHA
	}
	return out, mapErr(err)
}
