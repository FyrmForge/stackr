package service

import (
	"context"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// checkPanelResolves refuses a new panel host that does not point at the
// default ingress yet: the proxy would drop the old host and serve a name
// nothing reaches, locking the admin out. A host in a managed domain is
// pointed by stackr itself, and an ingress with no known public address, or
// no TLS, cannot be checked: both pass.
func (o *Orchestrator) checkPanelResolves(ctx context.Context, host string) error {
	if host == "" || o.cfg.TLSOff || o.dnsOwns(ctx, host) {
		return nil
	}
	if cur, _ := o.settings.Get(ctx, "panel_domain"); cur == host {
		return nil
	}
	id, err := o.DefaultIngress(ctx)
	if err != nil {
		return nil
	}
	m, err := o.ClusterMap(ctx)
	if err != nil {
		return nil
	}
	for _, c := range m.Nodes {
		if c.ID != id || len(c.Public) == 0 {
			continue
		}
		ips, err := resolveHost(ctx, host)
		if err != nil || len(ips) == 0 || slices.ContainsFunc(ips, func(ip string) bool { return !slices.Contains(c.Public, ip) }) {
			return errs.Invalidf("panel_domain", "%s does not point at this server yet.", host)
		}
	}
	return nil
}
