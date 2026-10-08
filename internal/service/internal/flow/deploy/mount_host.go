package deploy

import (
	"context"
	"strconv"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// HostLine is the grant-set form of a host mount: host:/a:/b[:ro].
func HostLine(m tile.Mount) string {
	return "host:" + m.Host + ":" + m.Path + roSuffix(m)
}

// HostSet is the elevated access tiles ask for, one "<slug> <perm>" line
// each: host mounts, devices, privileged, LAN access, published server ports
// and host networking. A line that does not parse is left out; the tile's
// own validation reports it.
func HostSet(ts ...store.Tile) hostgrant.Set {
	var s hostgrant.Set
	for _, t := range ts {
		add := func(perm string) { s.Lines = append(s.Lines, hostgrant.Line(t.Slug, perm)) }
		for _, l := range tile.Lines(t.Volumes) {
			if m, err := tile.ParseMount(l); err == nil && m.Kind == tile.MountHost {
				add(HostLine(m))
			}
		}
		for _, l := range tile.Lines(t.Devices) {
			add(hostgrant.Device + l)
		}
		for _, l := range tile.Lines(t.Lan) {
			if _, err := tile.ParseLAN(l); err == nil {
				add(hostgrant.LAN + l)
			}
		}
		for _, l := range tile.Lines(t.PublishedPorts) {
			if h, _, proto, err := tile.ParsePort(l); err == nil {
				p := hostgrant.Port + strconv.Itoa(h)
				if proto == "udp" {
					p += "/udp"
				}
				add(p)
			}
		}
		if t.Privileged {
			add(hostgrant.Privileged)
		}
		if t.HostNetwork {
			add(hostgrant.NetworkHost)
		}
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
// the stack's grant covers it for this tile. The lines were expanded before they got
// here, so a line built from a param is checked as it will run.
func (f *Flow) hostBind(ctx context.Context, st store.Stack, t store.Tile, m tile.Mount) (string, error) {
	if f.HostGrants == nil {
		return "", errs.Conflictf("%s: host mounts need the grant leaf", t.Slug)
	}
	if err := f.HostGrants.Check(ctx, st.ID, hostgrant.Set{Lines: []string{hostgrant.Line(t.Slug, HostLine(m))}}); err != nil {
		return "", err
	}
	return m.Host + ":" + m.Path + roSuffix(m), nil
}
