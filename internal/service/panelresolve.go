package service

import (
	"context"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

const panelAliasPrefix = "panel_alias."

// panelAliases are the panel's earlier hosts, which redirect to the current
// one. ponytail: kept until the row is deleted; no UI to drop one yet.
func (o *Orchestrator) panelAliases(ctx context.Context) []string {
	rows, _ := o.store.Settings.ListPrefix(ctx, panelAliasPrefix)
	var out []string
	for k := range rows {
		out = append(out, strings.TrimPrefix(k, panelAliasPrefix))
	}
	slices.Sort(out)
	return out
}

// keepPanelAlias remembers the host the panel just left (and forgets the one
// it came back to), so the old link still lands.
func (o *Orchestrator) keepPanelAlias(ctx context.Context, was, now string) {
	if was == now {
		return
	}
	_ = o.store.Settings.Delete(ctx, panelAliasPrefix+now)
	if was != "" {
		_ = o.store.Settings.Set(ctx, panelAliasPrefix+was, "1")
	}
}

// checkPanelResolves refuses a new panel host that does not point at the
// default ingress yet: the proxy would serve a name nothing reaches, locking
// the admin out (the old host stays as a redirect to it). A host in a managed domain is
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
