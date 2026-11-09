package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
)

const kindBucketRestore jobs.Kind = "bucket-restore"

// BucketSettings is a bucket slice's data protection, set as one group.
type BucketSettings struct {
	VersionDays *int     `json:"version_days"` // days old versions are kept; nil = 7, 0 = versioning off
	QuotaGB     *int     `json:"quota_gb"`     // hard size cap; nil or 0 = none
	CORSOrigins []string `json:"cors_origins"` // origins CORS allows; empty = none
}

// SetSliceBucket replaces slice tile sliceTileID's versioning, quota and CORS
// and writes them to its bucket now, with no deploy. A config-managed stack
// sets them in the stack file.
func (o *Orchestrator) SetSliceBucket(ctx context.Context, sliceTileID string, in BucketSettings) (Tile, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return s, err
	}
	st, err := o.stacks.Get(ctx, s.StackID)
	if err != nil {
		return s, err
	}
	if st.ConfigRepo != "" {
		return s, errs.Conflictf("stack %s is config-managed; set version_days, quota_gb and cors_origins in its stack file", st.Slug)
	}
	if _, ok, err := o.operated.ProvisionOf(ctx, s.ID); err != nil {
		return s, err
	} else if ok {
		if _, _, err := o.bucketOf(ctx, s); err != nil {
			return s, err
		}
	}
	cur := s
	cur.VersionDays, cur.QuotaGB, cur.CORSOrigins = in.VersionDays, in.QuotaGB, in.CORSOrigins
	out, _, err := o.tiles.Update(ctx, s, cur)
	if err != nil {
		return out, err
	}
	if ctx, err = o.instanceNode(ctx, out); err != nil {
		return out, err
	}
	return out, o.engines.Resync(ctx, out)
}

// bucketOf is slice tile s's provision, once it exists and is a bucket.
func (o *Orchestrator) bucketOf(ctx context.Context, s Tile) (Provision, OperatedInstance, error) {
	p, ok, err := o.operated.ProvisionOf(ctx, s.ID)
	if err != nil {
		return p, OperatedInstance{}, err
	}
	if !ok {
		return p, OperatedInstance{}, errs.Conflictf("slice %s has no bucket yet; deploy it first", s.Slug)
	}
	m, err := o.operated.Get(ctx, p.InstanceID)
	if err != nil {
		return p, m, err
	}
	if m.Engine != "s3" {
		return p, m, errs.Invalidf("slice", "slice %s is not a bucket", s.Slug)
	}
	return p, m, nil
}

// instanceNode puts ctx on the node of slice tile s's instance, if it has one.
func (o *Orchestrator) instanceNode(ctx context.Context, s Tile) (context.Context, error) {
	p, ok, err := o.operated.ProvisionOf(ctx, s.ID)
	if err != nil || !ok {
		return ctx, err
	}
	m, err := o.operated.Get(ctx, p.InstanceID)
	if err != nil {
		return ctx, err
	}
	return o.withTileNode(ctx, m.TileID)
}

// BucketUsage is the bytes of a bucket's current objects and, apart, of its
// old versions.
type BucketUsage struct {
	CurrentBytes int64 `json:"current_bytes"`
	OldBytes     int64 `json:"old_bytes"`
}

// SliceUsage reads the sizes off the bucket itself.
// ponytail: lists every version on each call; cache or ask the admin usage
// API when a bucket holds millions.
func (o *Orchestrator) SliceUsage(ctx context.Context, sliceTileID string) (BucketUsage, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return BucketUsage{}, err
	}
	p, _, err := o.bucketOf(ctx, s)
	if err != nil {
		return BucketUsage{}, err
	}
	if ctx, err = o.instanceNode(ctx, s); err != nil {
		return BucketUsage{}, err
	}
	cur, old, err := o.engines.Sizes(ctx, p)
	return BucketUsage{CurrentBytes: cur, OldBytes: old}, err
}

type bucketRestoreJob struct {
	TileID string    `json:"tile_id"` // the slice tile
	Prefix string    `json:"prefix"`
	Exact  bool      `json:"exact"`
	At     time.Time `json:"at"`
}

// RestoreSlice queues the job that copies, for the object named key (exact)
// or every object under prefix, the version current at at back on top as a
// new version. It never deletes: what did not exist at at is left alone. A
// whole-bucket restore is prefix "". It runs file by file, not atomically.
func (o *Orchestrator) RestoreSlice(ctx context.Context, sliceTileID, prefix string, exact bool, at time.Time) (Job, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return Job{}, err
	}
	if at.IsZero() || at.After(time.Now()) {
		return Job{}, errs.Invalidf("at", "pick a time in the past")
	}
	if exact && prefix == "" {
		return Job{}, errs.Invalidf("key", "name the object to restore")
	}
	if _, _, err := o.bucketOf(ctx, s); err != nil {
		return Job{}, err
	}
	// Each restore has its own lock entry besides the slice: two restores of
	// one slice run one after the other, neither supersedes the other.
	return o.enqueue(ctx, kindBucketRestore, bucketRestoreJob{TileID: s.ID, Prefix: prefix, Exact: exact, At: at.UTC()},
		s.ID, uuid.NewString())
}

func (o *Orchestrator) runBucketRestore(ctx context.Context, r *jobs.Run, p bucketRestoreJob) error {
	s, err := o.sliceTile(ctx, p.TileID)
	if err != nil {
		return err
	}
	prov, _, err := o.bucketOf(ctx, s)
	if err != nil {
		return err
	}
	if ctx, err = o.instanceNode(ctx, s); err != nil {
		return err
	}
	o.ensureMesh(ctx)
	return o.engines.RestoreBucket(ctx, prov, p.Prefix, p.Exact, p.At, r.Log)
}
