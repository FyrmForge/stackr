package deploy

import (
	"context"
	"errors"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// shareBind is a "share:slug/sub:/abs[:ro]" line: a Docker volume on the
// org's network share, as a Docker bind. The share is the tile's own org's;
// its user and password are ${{ org.params }} refs only (a share is an org
// object), read as the tile's env reads them (its tier's block, or an unlocked [x] one), expanded here, so a
// secret reaches Docker's volume opts and nothing else.
func (f *Flow) shareBind(ctx context.Context, o store.Org, e store.Environment, st store.Stack, t store.Tile, m tile.Mount) (string, error) {
	if t.Kind == tile.Managed {
		return "", errs.Conflictf("%s: a database on a share corrupts; give it a volume", t.Slug)
	}
	s, err := f.Volumes.ShareBySlug(ctx, o.ID, m.Share)
	if errors.Is(err, errs.ErrNotFound) {
		return "", errs.Conflictf("%s mounts share %q, which this organization does not have", t.Slug, m.Share)
	}
	if err != nil {
		return "", err
	}
	snap, err := f.ParamSnapshot(ctx, e, st, true)
	if err != nil {
		return "", err
	}
	for _, v := range []string{s.User, s.PasswordRef} {
		for _, b := range params.Refs(v) {
			if r, err := params.Parse(b); err == nil && r.Kind == params.KindParam {
				return "", errs.Conflictf("share %s: a share login reads org.params only", s.Slug)
			}
		}
	}
	rr := params.NewResolver(snap)
	user, err := rr.Expand(params.InEnv, s.User)
	if err != nil {
		return "", err
	}
	pass, err := rr.Expand(params.InEnv, s.PasswordRef)
	if err != nil {
		return "", err
	}
	name, err := f.Volumes.EnsureShare(ctx, s, m.Sub, user, pass)
	if err != nil {
		return "", err
	}
	return name + ":" + m.Path + roSuffix(m), nil
}
