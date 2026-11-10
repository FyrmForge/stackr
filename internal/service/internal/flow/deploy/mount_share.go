package deploy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// shareBind is a "share:slug/sub:/abs[:ro]" line: a Docker volume on the
// org's network share, as a Docker bind. The share is the tile's own org's;
// its user and password are ${{ org.params }} refs only (a share is an org
// object), read as the tile's env reads them (its tier's block, or an unlocked [x] one), expanded here, so a
// secret reaches Docker's volume opts and nothing else.
func (f *Flow) shareBind(ctx context.Context, o store.Org, e store.Environment, st store.Stack, t store.Tile, m tile.Mount) (string, error) {
	if t.Kind == tile.Operated {
		return "", errs.Conflictf("%s: a database on a share corrupts; give it a volume", t.Slug)
	}
	s, err := f.Volumes.ShareBySlug(ctx, o.ID, m.Share)
	if errors.Is(err, errs.ErrNotFound) {
		return "", errs.Conflictf("%s mounts share %q, which this organization does not have", t.Slug, m.Share)
	}
	if err != nil {
		return "", err
	}
	// The host mounts the share from its own network: an admin grant first,
	// checked on the expanded line too (a param may have written it).
	if f.HostGrants == nil {
		return "", errs.Conflictf("%s: share mounts need the grant leaf", t.Slug)
	}
	addr, err := f.shareAddr(ctx, s)
	if err != nil {
		return "", err
	}
	if err := f.HostGrants.Check(ctx, st.ID, hostgrant.Set{Lines: []string{hostgrant.Line(t.Slug, ShareGrant(s.Slug, addr))}}); err != nil {
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
	name, err := f.Volumes.EnsureShare(ctx, s, m.Sub, addr, user, pass)
	if err != nil {
		return "", err
	}
	return name + ":" + m.Path + roSuffix(m), nil
}

// ShareGrant is the perm an approval of mounting share slug at addr holds.
func ShareGrant(slug, addr string) string { return hostgrant.Share + slug + "@" + addr }

// shareAddr is the IP the node mounts for s: its source when that is an IP,
// else the first IPv4 its name resolves to in sorted order (an IPv6 only when
// there is none), so the pin is stable. No answer is an error, no mount.
func (f *Flow) shareAddr(ctx context.Context, s store.Share) (string, error) {
	h := volume.ShareHost(s)
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.String(), nil
	}
	lookup := f.LookupHost
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	as, err := lookup(ctx, h)
	var ips []netip.Addr
	for _, a := range as {
		if ip, perr := netip.ParseAddr(a); perr == nil {
			ips = append(ips, ip.Unmap())
		}
	}
	if len(ips) == 0 {
		return "", errs.Conflictf("share %s: cannot resolve %q to an address (%v)", s.Slug, h, err)
	}
	slices.SortFunc(ips, func(a, b netip.Addr) int {
		if a.Is4() != b.Is4() {
			if a.Is4() {
				return -1
			}
			return 1
		}
		return a.Compare(b)
	})
	return ips[0].String(), nil
}

// pinShares is set with its bare "share:slug" lines swapped for the pinned
// ones the deploy would ask for. A share that is gone or does not resolve
// keeps its bare line and its error is returned (the deploy reports it).
func (f *Flow) pinShares(ctx context.Context, st store.Stack, set hostgrant.Set) (hostgrant.Set, error) {
	var out hostgrant.Set
	var first error
	for _, l := range set.Lines {
		sl, p := hostgrant.Split(l)
		if !f.sharePerm(p) || f.Volumes == nil {
			out.Lines = append(out.Lines, l)
			continue
		}
		s, err := f.Volumes.ShareBySlug(ctx, st.OrgID, p[len(hostgrant.Share):])
		if err == nil {
			var addr string
			if addr, err = f.shareAddr(ctx, s); err == nil {
				l = hostgrant.Line(sl, ShareGrant(s.Slug, addr))
			}
		}
		if err != nil && first == nil && !errors.Is(err, errs.ErrNotFound) {
			first = err
		}
		out.Lines = append(out.Lines, l)
	}
	return out.Norm(), first
}

func (f *Flow) sharePerm(p string) bool {
	return strings.HasPrefix(p, hostgrant.Share) && !strings.Contains(p, "@")
}

// PinnedHostSet is HostSet with each share line pinned to its resolved
// address (see pinShares), for the callers that check a grant before a deploy.
func (f *Flow) PinnedHostSet(ctx context.Context, st store.Stack, ts ...store.Tile) (hostgrant.Set, error) {
	return f.pinShares(ctx, st, HostSet(ts...))
}
