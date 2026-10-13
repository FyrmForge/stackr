package store

import (
	"context"
	"time"
)

// RegistryImage is a row of registry_images: one version (repo and manifest
// digest) in the cluster registry, written by the registry leaf.
type RegistryImage struct {
	ID    string  `db:"id" json:"id"`
	OrgID *string `db:"org_id" json:"org_id"` // nil for stackr's own images
	Repo  string  `db:"repo" json:"repo"`
	Tag   string  `db:"tag" json:"tag"`
	// Digest is the manifest (index) digest; for an import it is the upstream's.
	Digest string `db:"digest" json:"digest"`
	// Platforms are the CPU types behind the digest, comma separated.
	Platforms string `db:"platforms" json:"platforms"`
	// Source is "built", "imported" or "system".
	Source      string    `db:"source" json:"source"`
	UpstreamRef string    `db:"upstream_ref" json:"upstream_ref"` // "" for builds
	Size        int64     `db:"size" json:"size"`
	CreatedBy   string    `db:"created_by" json:"created_by"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type RegistryImageStore interface {
	Create(ctx context.Context, r RegistryImage) error
	Get(ctx context.Context, id string) (RegistryImage, error)
	GetByRepoDigest(ctx context.Context, repo, digest string) (RegistryImage, error)
	// ListByOrg is the org's versions, newest first.
	ListByOrg(ctx context.Context, orgID string) ([]RegistryImage, error)
	// ListByRepo is one repo's versions, newest first.
	ListByRepo(ctx context.Context, repo string) ([]RegistryImage, error)
	// ListAll is every version, system images included, newest first.
	ListAll(ctx context.Context) ([]RegistryImage, error)
	Update(ctx context.Context, r RegistryImage) error
	Delete(ctx context.Context, id string) error
	// DeleteByRepoDigest forgets a version; a version with no row is not an error.
	DeleteByRepoDigest(ctx context.Context, repo, digest string) error
}

var registryImagesT = newTable[RegistryImage]("registry_images", nil)

type registryImages struct{ crud[RegistryImage] }

func (s registryImages) GetByRepoDigest(ctx context.Context, repo, digest string) (RegistryImage, error) {
	return s.one(ctx, "repo = ? AND digest = ?", repo, digest)
}

func (s registryImages) ListByOrg(ctx context.Context, orgID string) ([]RegistryImage, error) {
	return s.many(ctx, "org_id = ? ORDER BY created_at DESC, id", orgID)
}

func (s registryImages) ListByRepo(ctx context.Context, repo string) ([]RegistryImage, error) {
	return s.many(ctx, "repo = ? ORDER BY created_at DESC, id", repo)
}

func (s registryImages) ListAll(ctx context.Context) ([]RegistryImage, error) {
	return s.many(ctx, "1 = 1 ORDER BY created_at DESC, id")
}

func (s registryImages) DeleteByRepoDigest(ctx context.Context, repo, digest string) error {
	_, err := s.q.ExecContext(ctx, "DELETE FROM registry_images WHERE repo = ? AND digest = ?", repo, digest)
	return mapErr(err)
}
