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
	"slices"
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
	ListVolumes(ctx context.Context, labels map[string]string) ([]docker.VolumeInfo, error)
	InspectVolume(ctx context.Context, name string) (docker.VolumeInfo, error)
	EnsureTool(ctx context.Context) error
	TarVolume(ctx context.Context, name string, w io.Writer, live bool) error
	UntarVolume(ctx context.Context, name string, src io.Reader) error
}

// Scope is where a volume lives: env, stack or org, and that row's id.
type Scope struct{ Kind, ID string }

type Leaf struct {
	volumes store.VolumeStore
	shares  store.ShareStore // nil until WithShares; network shares live in share.go
	docker  Docker
	// Node, when set, is the node a scope's volumes live on: an env's own
	// node; "" (a stack, an org, a share) is the controller.
	Node func(ctx context.Context, scopeKind, scopeID string) (nodeID string, err error)
}

// WithNode sets how the leaf finds the node a scope's volumes live on.
func (l *Leaf) WithNode(f func(ctx context.Context, scopeKind, scopeID string) (string, error)) *Leaf {
	l.Node = f
	return l
}

// at is ctx for Docker calls on the node v lives on. A NodeID wins over the
// scope lookup: an orphan's env row is gone, and a volume with no row at all
// (a move leftover) only knows its node.
func (l *Leaf) at(ctx context.Context, v store.Volume) (context.Context, error) {
	if v.NodeID != "" {
		return docker.WithNode(ctx, v.NodeID), nil
	}
	if l.Node == nil {
		return ctx, nil
	}
	id, err := l.Node(ctx, v.ScopeKind, v.ScopeID)
	if err != nil {
		return ctx, err
	}
	return docker.WithNode(ctx, id), nil
}

// orController keeps the node ctx already names, else the controller's: a
// share belongs to an org, its volumes to whichever node mounts them.
// ponytail: a share's volumes on workers are not swept until nodes can be
// walked from here; deploys create them on the env's node via ctx.
func (l *Leaf) orController(ctx context.Context) (context.Context, error) {
	if l.Node == nil {
		return ctx, nil
	}
	if _, ok := docker.NodeFrom(ctx); ok {
		return ctx, nil
	}
	id, err := l.Node(ctx, "", "")
	if err != nil {
		return ctx, err
	}
	return docker.WithNode(ctx, id), nil
}

func New(volumes store.VolumeStore, d Docker) *Leaf { return &Leaf{volumes: volumes, docker: d} }

func (l *Leaf) Get(ctx context.Context, id string) (store.Volume, error) {
	return l.volumes.Get(ctx, id)
}

func (l *Leaf) List(ctx context.Context, s Scope) ([]store.Volume, error) {
	return l.volumes.ListByScope(ctx, s.Kind, s.ID)
}

func (l *Leaf) BySlug(ctx context.Context, s Scope, sl string) (store.Volume, error) {
	vs, err := l.List(ctx, s)
	if err != nil {
		return store.Volume{}, err
	}
	i := slices.IndexFunc(vs, func(v store.Volume) bool { return v.Slug == sl })
	if i < 0 {
		return store.Volume{}, errs.ErrNotFound
	}
	return vs[i], nil
}

// Declare is the row for a volume the stack file (or an instance) names.
// An orphan with the same slug in the same scope is re-adopted, data and
// all; adopted says so. instanceID is the owning managed instance, if any.
func (l *Leaf) Declare(
	ctx context.Context,
	s Scope,
	sl string,
	maxSizeMB int,
	instanceID *string,
) (v store.Volume, adopted bool, err error) {
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
		v = store.Volume{
			ID:         id,
			ScopeKind:  s.Kind,
			ScopeID:    s.ID,
			InstanceID: instanceID,
			Slug:       sl,
			Name:       "stackr-vol-" + id,
			MaxSizeMB:  maxSizeMB,
			CreatedAt:  time.Now().UTC(),
		}
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
	ctx, err := l.at(ctx, v)
	if err != nil {
		return v.Name, err
	}
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
	ctx, err := l.at(ctx, v)
	if err != nil {
		return true
	}
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
	if err := detached(mountedBy); err != nil {
		return err
	}
	ctx, err := l.at(ctx, v)
	if err != nil {
		return err
	}
	if err := l.docker.RemoveVolume(ctx, v.Name); err != nil {
		return err
	}
	return l.volumes.Delete(ctx, v.ID)
}

// DeleteRow is Delete for a volume whose node is gone: the row only.
func (l *Leaf) DeleteRow(ctx context.Context, v store.Volume, mountedBy []string) error {
	if err := detached(mountedBy); err != nil {
		return err
	}
	return l.volumes.Delete(ctx, v.ID)
}

func detached(mountedBy []string) error {
	if len(mountedBy) > 0 {
		return errs.Conflictf("detach the volume from %s before deleting it", strings.Join(mountedBy, ", "))
	}
	return nil
}

// SizeBytes is the volume's uncompressed size; -1 when Docker cannot tell.
func (l *Leaf) SizeBytes(ctx context.Context, v store.Volume) int64 {
	ctx, err := l.at(ctx, v)
	if err != nil {
		return -1
	}
	info, err := l.docker.InspectVolume(ctx, v.Name)
	if err != nil {
		return -1
	}
	return info.SizeBytes
}

// EnsureTool pulls the tar helper on the volume's node; call it before freezing anything.
func (l *Leaf) EnsureTool(ctx context.Context, v store.Volume) error {
	ctx, err := l.at(ctx, v)
	if err != nil {
		return err
	}
	return l.docker.EnsureTool(ctx)
}

// Tar streams a gzipped tar of the volume into w.
func (l *Leaf) Tar(ctx context.Context, v store.Volume, w io.Writer, live bool) error {
	ctx, err := l.at(ctx, v)
	if err != nil {
		return err
	}
	return l.docker.TarVolume(ctx, v.Name, w, live)
}

// Untar WIPES the volume and extracts src into it. The caller verifies the
// archive first: there is no way back from the wipe.
func (l *Leaf) Untar(ctx context.Context, v store.Volume, src io.Reader) error {
	ctx, err := l.at(ctx, v)
	if err != nil {
		return err
	}
	return l.docker.UntarVolume(ctx, v.Name, src)
}
