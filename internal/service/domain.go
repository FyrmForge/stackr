package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	Domain       = store.Domain
	DomainSpec   = domain.Spec // RawCaddy is admin-only: only SetRawCaddy writes it
	DomainExtras = domain.Extras
)

func (o *Orchestrator) Domains(ctx context.Context, tileID string) ([]Domain, error) {
	ds, err := o.domains.ListByTile(ctx, tileID)
	for i := range ds {
		ds[i] = redact(ds[i])
	}
	return ds, err
}

// redact blanks a basic-auth password on the way out: reads never carry a
// secret. UpdateDomain keeps the stored password when the same user comes
// back with an empty one, so a read-then-write round trip changes nothing.
func redact(d Domain) Domain {
	var x DomainExtras
	if json.Unmarshal([]byte(d.ProxyJSON), &x) != nil || x.BasicAuth == nil || x.BasicAuth.Password == "" {
		return d
	}
	x.BasicAuth.Password = ""
	if b, err := json.Marshal(x); err == nil {
		d.ProxyJSON = string(b)
	}
	return d
}

// AttachDomain adds a host to a tile and pushes the proxy config. Port 0 =
// the tile's container port. RawCaddy is ignored: SetRawCaddy writes it.
// Auto takes the tile's generated name under the nearest domain resource
// visible to its stack and records that resource; the host given is
// ignored. Otherwise the host is a literal: no resource named it, and it
// takes the squat check (a generated name's first label is the tile's
// slug, which may be another org's).
func (o *Orchestrator) AttachDomain(ctx context.Context, tileID string, s DomainSpec) (Domain, error) {
	s.RawCaddy = ""
	s.ResourceID = nil
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return Domain{}, err
	}
	if tile.RunToCompletion(t.Kind) {
		return Domain{}, errs.Invalidf("domains", "a %s has no endpoint; domains do not apply", t.Kind)
	}
	if s.Auto {
		host, res, err := o.autoHost(ctx, t)
		if err != nil {
			return Domain{}, err
		}
		s.Host = host
		s.ResourceID = &res.ID
	} else if err := o.checkSquat(ctx, t, s.Host); err != nil {
		return Domain{}, err
	}
	if s.Port == 0 {
		s.Port = t.ContainerPort
	}
	cs, err := o.tiles.Replicas(ctx, t)
	if err != nil {
		return Domain{}, err
	}
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	d, err := o.domains.Attach(ctx, tileID, s, o.dns01(ctx), ids)
	if err != nil {
		return d, err
	}
	return redact(d), o.sync.Sync(ctx)
}

// UpdateDomain replaces the domain's spec; the stored raw Caddy route stays
// (only SetRawCaddy, an admin verb, writes it), and so do the resource that
// named the host and its auto mark while the host stays. Only a new host
// takes the squat check: a kept one passed it, or was generated.
func (o *Orchestrator) UpdateDomain(ctx context.Context, id string, s DomainSpec) (Domain, error) {
	d, err := o.domains.Get(ctx, id)
	if err != nil {
		return d, err
	}
	t, err := o.tiles.Get(ctx, d.TileID)
	if err != nil {
		return d, err
	}
	sameHost := strings.EqualFold(strings.TrimSpace(s.Host), d.Host)
	if !sameHost {
		if err := o.checkSquat(ctx, t, s.Host); err != nil {
			return d, err
		}
	}
	if s.Port == 0 {
		s.Port = d.ContainerPort
	}
	s.RawCaddy = d.RawCaddy
	s.ResourceID = nil
	s.Auto = false
	if sameHost {
		s.ResourceID = d.ResourceID
		s.Auto = d.Auto
	}
	if a := s.Extras.BasicAuth; a != nil && a.Password == "" {
		var old DomainExtras
		if json.Unmarshal([]byte(d.ProxyJSON), &old) == nil && old.BasicAuth != nil && old.BasicAuth.User == a.User {
			a.Password = old.BasicAuth.Password
		}
	}
	if d, err = o.domains.Update(ctx, d, s, o.dns01(ctx)); err != nil {
		return d, err
	}
	return redact(d), o.sync.Sync(ctx)
}

// SetRawCaddy replaces the domain's generated route with raw, verbatim; ""
// goes back to the generated one. Admin only.
func (o *Orchestrator) SetRawCaddy(ctx context.Context, id, raw string) (Domain, error) {
	d, err := o.domains.Get(ctx, id)
	if err != nil {
		return d, err
	}
	s, err := specOf(d)
	if err != nil {
		return d, err
	}
	s.RawCaddy = raw
	if d, err = o.domains.Update(ctx, d, s, o.dns01(ctx)); err != nil {
		return d, err
	}
	return redact(d), o.sync.Sync(ctx)
}

// specOf is a stored row as the spec that writes it back unchanged: its raw
// route, auto mark and resource included. d must not be redacted.
func specOf(d Domain) (DomainSpec, error) {
	x, err := domain.ExtrasOf(d)
	s := DomainSpec{
		Host:       d.Host,
		Path:       d.Path,
		Port:       d.ContainerPort,
		HTTPS:      &d.HTTPS,
		ForceHTTPS: &d.ForceHTTPS,
		RedirectTo: d.RedirectTo,
		Auto:       d.Auto,
		ResourceID: d.ResourceID,
		Extras:     x,
		RawCaddy:   d.RawCaddy,
	}
	return s, err
}

// refreshAutoHosts runs after a rename moved a slug an auto domain's host
// is made of (org, stack, env or tile): each auto row of ts takes the host
// AttachDomain would give it now, and the proxy config is pushed once if
// one moved. Nothing redeploys: the route is all that names the host.
// ponytail: only renames call it. A reorder that moves the default env, or
// a domain resource added or removed that changes the nearest one, leaves
// the host until the stack's next promote. Under the org's own resource the
// org slug is in the resource's host, which a rename does not move.
func (o *Orchestrator) refreshAutoHosts(ctx context.Context, ts []Tile) error {
	moved := false
	for _, t := range ts {
		ds, err := o.domains.ListByTile(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, d := range ds {
			if !d.Auto {
				continue
			}
			host, res, err := o.autoHost(ctx, t)
			if err != nil {
				return err
			}
			if host == d.Host {
				continue
			}
			s, err := specOf(d)
			if err != nil {
				return err
			}
			s.Host = host
			s.ResourceID = &res.ID
			if _, err := o.domains.Update(ctx, d, s, o.dns01(ctx)); err != nil {
				return err
			}
			moved = true
		}
	}
	if !moved {
		return nil
	}
	return o.sync.Sync(ctx)
}

func (o *Orchestrator) DetachDomain(ctx context.Context, id string) error {
	d, err := o.domains.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := o.domains.Detach(ctx, d); err != nil {
		return err
	}
	return o.sync.Sync(ctx)
}

// SyncProxy rebuilds and pushes the whole proxy config now.
func (o *Orchestrator) SyncProxy(ctx context.Context) error { return o.sync.Sync(ctx) }

// checkSquat refuses a host leading with another org's slug; the tile's own
// org never counts against it (leaf/domainres CheckOrgSquat).
func (o *Orchestrator) checkSquat(ctx context.Context, t Tile, host string) error {
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return err
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	return domainres.CheckOrgSquat(strings.ToLower(strings.TrimSpace(host)), st.OrgID, orgs)
}

func (o *Orchestrator) dns01(ctx context.Context) bool {
	p, _ := o.settings.Get(ctx, "dns_provider")
	return p != ""
}

// Route is one domain the env's proxy serves, with the tile it lands on.
type Route struct {
	Domain
	Tile string `json:"tile"` // the tile's name
}

// Routes are the env's domains, every tile's, in tile order (the proxy
// card's drawer).
func (o *Orchestrator) Routes(ctx context.Context, envID string) ([]Route, error) {
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return nil, err
	}
	var out []Route
	for _, t := range ts {
		ds, err := o.Domains(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			out = append(out, Route{d, t.Name})
		}
	}
	return out, nil
}
