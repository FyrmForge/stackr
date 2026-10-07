package deploy

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// HostLine is the grant-set form of a host mount: host:/a:/b[:ro].
func HostLine(m tile.Mount) string {
	return "host:" + m.Host + ":" + m.Path + roSuffix(m)
}

// HostSet is the host access tiles ask for: their host mount lines, device
// lines and privileged flag. A line that does not parse is left out; the
// tile's own validation reports it.
func HostSet(ts ...store.Tile) hostgrant.Set {
	var s hostgrant.Set
	for _, t := range ts {
		for _, l := range tile.Lines(t.Volumes) {
			if m, err := tile.ParseMount(l); err == nil && m.Kind == tile.MountHost {
				s.Lines = append(s.Lines, HostLine(m))
			}
		}
		for _, l := range tile.Lines(t.Devices) {
			s.Lines = append(s.Lines, hostgrant.Device+l)
		}
		s.Privileged = s.Privileged || t.Privileged
	}
	return s.Norm()
}

// checkAccess parks (errs.NeedsApproval) a tile whose host access the
// stack's grant does not cover, before any container changes. It returns
// what the grant allows.
func (f *Flow) checkAccess(ctx context.Context, st store.Stack, t store.Tile) (hostgrant.Set, error) {
	if f.HostGrants == nil {
		return hostgrant.Set{}, nil
	}
	if err := f.HostGrants.Check(ctx, st.ID, HostSet(t)); err != nil {
		return hostgrant.Set{}, err
	}
	return f.HostGrants.Of(ctx, st.ID)
}

// hostBind is a "host:/a:/b[:ro]" line: a server path as a Docker bind, once
// the stack's host grant covers it. The lines were expanded before they got
// here, so a line built from a param is checked as it will run.
func (f *Flow) hostBind(ctx context.Context, st store.Stack, t store.Tile, m tile.Mount) (string, error) {
	if f.HostGrants == nil {
		return "", errs.Conflictf("%s: host mounts need the grant leaf", t.Slug)
	}
	if err := f.HostGrants.Check(ctx, st.ID, hostgrant.Set{Lines: []string{HostLine(m)}}); err != nil {
		return "", err
	}
	return m.Host + ":" + m.Path + roSuffix(m), nil
}
