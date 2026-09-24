// Package volume owns volumes: the row and the Docker volume. A volume is
// scoped (env, or the scope of the managed instance that owns it) and is
// never removed by a deploy, rollback, promote or tile delete. It is orphaned
// (row and data stay) when its entry leaves the stack file or its instance
// goes; declaring the same slug again re-adopts it. Only an explicit Delete
// removes data.
package volume

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Docker interface {
	CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	InspectVolume(ctx context.Context, name string) (docker.VolumeInfo, error)
	EnsureTool(ctx context.Context) error
	TarVolume(ctx context.Context, name string, w io.Writer, live bool) error
	UntarVolume(ctx context.Context, name string, src io.Reader) error
}

// Scope is where a volume lives: env, stack or org, and that row's id.
type Scope struct{ Kind, ID string }

type Leaf struct {
	volumes store.VolumeStore
	docker  Docker
}

func New(volumes store.VolumeStore, d Docker) *Leaf { return &Leaf{volumes: volumes, docker: d} }

func (l *Leaf) Get(ctx context.Context, id string) (store.Volume, error) { return l.volumes.Get(ctx, id) }

func (l *Leaf) List(ctx context.Context, s Scope) ([]store.Volume, error) {
	return l.volumes.ListByScope(ctx, s.Kind, s.ID)
}

func (l *Leaf) BySlug(ctx context.Context, s Scope, sl string) (store.Volume, error) {
	vs, err := l.List(ctx, s)
	if err != nil {
		return store.Volume{}, err
	}
	for _, v := range vs {
		if v.Slug == sl {
			return v, nil
		}
	}
	return store.Volume{}, errs.ErrNotFound
}

// Declare is the row for a volume the stack file (or an instance) names.
// An orphan with the same slug in the same scope is re-adopted, data and
// all; adopted says so. instanceID is the owning managed instance, if any.
func (l *Leaf) Declare(ctx context.Context, s Scope, sl string, maxSizeMB int, instanceID *string) (v store.Volume, adopted bool, err error) {
	if !slug.Valid(sl) {
		return v, false, errs.Invalidf("volume", "%q: a volume name is lower-case letters, digits and single hyphens", sl)
	}
	if maxSizeMB < 0 {
		return v, false, errs.Invalidf("max_size_mb", "must not be negative")
	}
	v, err = l.BySlug(ctx, s, sl)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		id := uuid.NewString()
		v = store.Volume{ID: id, ScopeKind: s.Kind, ScopeID: s.ID, InstanceID: instanceID, Slug: sl,
			Name: "stackr-vol-" + id, MaxSizeMB: maxSizeMB, CreatedAt: time.Now().UTC()}
		return v, false, l.volumes.Create(ctx, v)
	case err != nil:
		return v, false, err
	}
	adopted = v.OrphanedAt != nil
	v.OrphanedAt, v.MaxSizeMB, v.InstanceID = nil, maxSizeMB, instanceID
	return v, adopted, l.volumes.Update(ctx, v)
}

// Ensure makes the Docker volume; an existing one is fine. Returns its name.
// ponytail: max_size_mb is recorded, not enforced; the local driver has no
// quota.
func (l *Leaf) Ensure(ctx context.Context, v store.Volume) (string, error) {
	return v.Name, l.docker.CreateVolume(ctx, v.Name, "local", nil, map[string]string{"stackr.volume": v.ID})
}

// Orphan flags the volume; row and data stay. Idempotent: the first
// timestamp is the one retention counts from.
func (l *Leaf) Orphan(ctx context.Context, v store.Volume) (store.Volume, error) {
	if v.OrphanedAt != nil {
		return v, nil
	}
	now := time.Now().UTC()
	v.OrphanedAt = &now
	return v, l.volumes.Update(ctx, v)
}

// Expired are the orphans older than retention: the daily job backs each up
// (when HoldsData) and then deletes it.
func (l *Leaf) Expired(ctx context.Context, retention time.Duration, now time.Time) ([]store.Volume, error) {
	vs, err := l.volumes.ListOrphaned(ctx)
	out := vs[:0]
	for _, v := range vs {
		if v.OrphanedAt.Add(retention).Before(now) {
			out = append(out, v)
		}
	}
	return out, err
}

// HoldsData is the one answer to "is there anything to lose". Fails safe: a
// size Docker cannot tell (-1) or an inspect error counts as data; only a
// volume that is gone or measured empty does not.
func (l *Leaf) HoldsData(ctx context.Context, v store.Volume) bool {
	info, err := l.docker.InspectVolume(ctx, v.Name)
	if errors.Is(err, docker.ErrNotFound) {
		return false
	}
	return err != nil || info.SizeBytes != 0
}

// Delete removes the Docker volume and the row: explicit, from the UI or
// CLI, or the orphan job after its backup. mountedBy are the slugs of the
// tiles whose volumes lines still name it.
func (l *Leaf) Delete(ctx context.Context, v store.Volume, mountedBy []string) error {
	if len(mountedBy) > 0 {
		return errs.Conflictf("detach the volume from %s before deleting it", strings.Join(mountedBy, ", "))
	}
	if err := l.docker.RemoveVolume(ctx, v.Name); err != nil {
		return err
	}
	return l.volumes.Delete(ctx, v.ID)
}

// SizeBytes is the volume's uncompressed size; -1 when Docker cannot tell.
func (l *Leaf) SizeBytes(ctx context.Context, v store.Volume) int64 {
	info, err := l.docker.InspectVolume(ctx, v.Name)
	if err != nil {
		return -1
	}
	return info.SizeBytes
}

// EnsureTool pulls the tar helper; call it before freezing anything.
func (l *Leaf) EnsureTool(ctx context.Context) error { return l.docker.EnsureTool(ctx) }

// Tar streams a gzipped tar of the volume into w.
func (l *Leaf) Tar(ctx context.Context, v store.Volume, w io.Writer, live bool) error {
	return l.docker.TarVolume(ctx, v.Name, w, live)
}

// Untar WIPES the volume and extracts src into it. The caller verifies the
// archive first: there is no way back from the wipe.
func (l *Leaf) Untar(ctx context.Context, v store.Volume, src io.Reader) error {
	return l.docker.UntarVolume(ctx, v.Name, src)
}
