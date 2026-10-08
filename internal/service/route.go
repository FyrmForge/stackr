package service

import (
	"context"
	"slices"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/route"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	// ExternalRoute is a host that goes to an address outside stackr
	// (admin-made, server-wide). Named so: Route is a tile domain row.
	ExternalRoute     = store.Route
	ExternalRouteSpec = route.Spec
)

// ExternalRoutes is every external route, by host (admin).
func (o *Orchestrator) ExternalRoutes(ctx context.Context) ([]ExternalRoute, error) {
	return o.routes.List(ctx)
}

// CreateExternalRoute adds a route and pushes the proxy. The host may not
// be a tile domain's or a domain resource's, and the reverse is checked by
// checkSquat.
func (o *Orchestrator) CreateExternalRoute(ctx context.Context, s ExternalRouteSpec) (ExternalRoute, error) {
	taken, err := o.domainHosts(ctx)
	if err != nil {
		return ExternalRoute{}, err
	}
	if s.Mode == route.Passthrough {
		if err := o.checkPanelHost(ctx, s.Host); err != nil {
			return ExternalRoute{}, err
		}
	}
	r, err := o.routes.Create(ctx, s, taken, !o.cfg.TLSOff, o.dns01(ctx))
	if err != nil {
		return r, err
	}
	return r, o.sync.Sync(ctx)
}

// DeleteExternalRoute removes a route and pushes the proxy.
func (o *Orchestrator) DeleteExternalRoute(ctx context.Context, id string) error {
	if _, err := o.routes.Get(ctx, id); err != nil {
		return err
	}
	if err := o.routes.Delete(ctx, id); err != nil {
		return err
	}
	return o.sync.Sync(ctx)
}

// domainHosts is every host the tile domains and domain resources hold.
func (o *Orchestrator) domainHosts(ctx context.Context) ([]string, error) {
	ds, err := o.domains.List(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := o.domainres.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(ds)+len(rs))
	for _, d := range ds {
		hosts = append(hosts, d.Host)
	}
	for _, r := range rs {
		hosts = append(hosts, r.Host)
	}
	return hosts, nil
}

// checkPanelHost refuses a pass-through route over the panel's host: layer4
// answers before HTTP, so the panel would be unreachable.
func (o *Orchestrator) checkPanelHost(ctx context.Context, host string) error {
	panel, _ := o.settings.Get(ctx, "panel_domain")
	panel = installspec.CleanHost(panel)
	if panel == "" {
		return nil
	}
	if h := installspec.CleanHost(host); route.Overlap(h, panel) {
		return errs.Conflictf("%s would lock the panel out: it answers on %s.", h, panel)
	}
	return nil
}

// checkPanelTaken refuses a tile domain on the panel's own host: the panel
// vhost sorts first, so the tile would be dead.
func (o *Orchestrator) checkPanelTaken(ctx context.Context, host string) error {
	panel, _ := o.settings.Get(ctx, "panel_domain")
	if panel = installspec.CleanHost(panel); panel != "" && installspec.CleanHost(host) == panel {
		return errs.Conflictf("%s is the panel's address.", panel)
	}
	return nil
}

// checkPanelRoutes is the reverse: a panel_domain a pass-through route
// covers, or a tile domain already holds, is refused. The value it already
// has passes (a save that changes nothing must not fail).
func (o *Orchestrator) checkPanelRoutes(ctx context.Context, panel string) error {
	panel = installspec.CleanHost(panel)
	if panel == "" {
		return nil
	}
	cur, _ := o.settings.Get(ctx, "panel_domain")
	if installspec.CleanHost(cur) == panel {
		return nil
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(ds, func(d store.Domain) bool { return installspec.CleanHost(d.Host) == panel }) {
		return errs.Conflictf("%s is a tile domain: the panel would shadow it.", panel)
	}
	rows, err := o.routes.List(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Mode == route.Passthrough && route.Overlap(r.Host, panel) {
			return errs.Conflictf("The pass-through route for %s covers %s: the panel would be unreachable.", r.Host, panel)
		}
	}
	return nil
}

// checkRouteHost refuses a tile domain on a host an external route holds.
func (o *Orchestrator) checkRouteHost(ctx context.Context, host string) error {
	host = installspec.CleanHost(host)
	held, err := o.routes.Covers(ctx, host)
	if err != nil {
		return err
	}
	if held {
		return errs.Conflictf("%s is an external route.", host)
	}
	return nil
}

// externalRoutes is the rows as the proxy config's input.
func (o *Orchestrator) externalRoutes(ctx context.Context) ([]domain.Route, error) {
	rows, err := o.routes.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Route, len(rows))
	for i, r := range rows {
		out[i] = domain.Route{Host: r.Host, Mode: r.Mode, Target: r.Target, Insecure: r.Insecure}
	}
	return out, nil
}
