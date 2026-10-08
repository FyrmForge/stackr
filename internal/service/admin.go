package service

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	APIKey = store.APIKey
	Knob   = settings.Knob
	// SettingsBlob is one rung of the settings cascade.
	SettingsBlob = settings.Settings
)

// ---- users and keys ----

func (o *Orchestrator) Users(ctx context.Context) ([]User, error) { return o.users.List(ctx) }

func (o *Orchestrator) ChangePassword(ctx context.Context, userID, current, next string) error {
	return o.users.ChangePassword(ctx, userID, current, next)
}

// SetTheme is the user's own look: system, light or dark.
func (o *Orchestrator) SetTheme(ctx context.Context, userID, theme string) error {
	return o.users.SetTheme(ctx, userID, theme)
}

// SetPassword is the admin reset.
func (o *Orchestrator) SetPassword(ctx context.Context, userID, next string) error {
	return o.users.SetPassword(ctx, userID, next)
}

// MintKey makes an API key bound to orgID with the minter's live role
// there (B36); orgID "" is an unbound, admin-only key. The token is shown once.
func (o *Orchestrator) MintKey(ctx context.Context, userID, orgID, name string) (string, APIKey, error) {
	u, err := o.users.Get(ctx, userID)
	if err != nil {
		return "", APIKey{}, err
	}
	role := ""
	if orgID != "" {
		roles, err := o.orgs.Roles(ctx, userID)
		if err != nil {
			return "", APIKey{}, err
		}
		if role = roles[orgID]; role == "" {
			return "", APIKey{}, errs.ErrNotFound
		}
	}
	return o.users.MintKey(ctx, u, orgID, role, name)
}

func (o *Orchestrator) Keys(ctx context.Context, userID string) ([]APIKey, error) {
	return o.users.Keys(ctx, userID)
}

func (o *Orchestrator) RevokeKey(ctx context.Context, userID, keyID string) error {
	return o.users.RevokeKey(ctx, userID, keyID)
}

// ---- settings: the one catalogue ----

// Settings is the catalogue every surface enumerates.
func (o *Orchestrator) Settings() []Knob { return settings.Catalogue }

// Setting is a flat knob's effective value.
func (o *Orchestrator) Setting(ctx context.Context, key string) (string, error) {
	return o.settings.Get(ctx, key)
}

// scheduleKnobs are the settings the cron table is built from.
var scheduleKnobs = map[string]bool{
	"panel_backup_enabled":  true,
	"panel_backup_schedule": true,
	"cleanup_schedule":      true,
	"cleanup_enabled":       true,
	"orphans_enabled":       true,
	"orphans_schedule":      true,
}

// SetSetting writes one flat knob; see SetSettings. The server file's binding
// (server_config_*) is bind's: it checks the connector, this would not.
func (o *Orchestrator) SetSetting(ctx context.Context, key, raw string) error {
	if slices.ContainsFunc(settings.Catalogue, func(k Knob) bool { return k.Key == key && k.ConfigOnly }) {
		return errs.Invalidf(key, "%s is set by binding the server file (PUT /admin/config-repo)", key)
	}
	return o.SetSettings(ctx, map[string]string{key: raw})
}

// SetSettings writes flat knobs as one save: every value is checked before
// any is written (a typo is refused, B24), the schedule reloads once if a
// schedule knob changed, and the proxy is re-pushed once, since several flat
// knobs live in its config; workers and the watch interval are re-read live.
// Saves run one at a time: the proxy_custom take-back must not undo another
// save's value. The write order is fixed: root_domain (it renames the hosts
// the panel is checked against), panel_domain, the rest, proxy_custom last (so
// a failed write before it leaves nothing for the take-back).
func (o *Orchestrator) SetSettings(ctx context.Context, vals map[string]string) error {
	// ponytail: one process-wide save lock, in the repo lock map; per-key locks if saves ever contend.
	m, _ := o.repoLocks.LoadOrStore("settings", &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	vals = maps.Clone(vals)
	for key, raw := range vals {
		if err := settings.Check(key, raw); err != nil {
			return err
		}
		if key == "acme_email" || key == "dns_provider" {
			// what the server file's Parse refuses, so export then plan reads clean
			if _, err := serverconfig.CheckSetting(key, raw); err != nil {
				return errs.Invalidf(key, "%s", err.Error())
			}
		}
		if key == "trusted_proxies" {
			for _, r := range splitList(raw) {
				if _, err := netip.ParsePrefix(r); err != nil {
					if _, err := netip.ParseAddr(r); err != nil {
						// ponytail: no "cloudflare" keyword; list its ranges by hand until the proxy fetches them.
						return errs.Invalidf("trusted_proxies", "%q is not an IP or CIDR.", r)
					}
				}
			}
		}
		if key == "panel_domain" {
			host, err := o.cleanPanelDomain(ctx, raw)
			if err != nil {
				return err
			}
			vals[key] = host
			if err := o.checkPanelRoutes(ctx, host); err != nil {
				return err
			}
		}
		if key == "root_domain" {
			if err := o.checkRootDomain(ctx, raw); err != nil {
				return err
			}
		}
	}
	if raw, ok := vals["root_domain"]; ok && vals["panel_domain"] != "" {
		if err := o.checkPanelAgainstRename(ctx, raw, vals["panel_domain"]); err != nil {
			return err
		}
	}
	_, custom := vals["proxy_custom"]
	was, _ := o.settings.Get(ctx, "proxy_custom")
	reload := false
	rank := func(k string) int {
		switch k {
		case "root_domain":
			return 0
		case "panel_domain":
			return 1
		case "proxy_custom":
			return 3
		}
		return 2
	}
	keys := slices.SortedFunc(maps.Keys(vals), func(a, b string) int { return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b)) })
	for _, key := range keys {
		raw := vals[key]
		write := func() error { return o.settings.Set(ctx, key, raw) }
		if key == "root_domain" {
			write = func() error { return o.setRootDomain(ctx, raw) } // renames the instance domain resource
		}
		if err := write(); err != nil {
			return err
		}
		reload = reload || scheduleKnobs[key]
	}
	if reload {
		o.sched.Reload(ctx)
	}
	err := o.sync.Sync(ctx)
	if err != nil && custom {
		// Caddy checks proxy_custom only at the push, after the row is
		// written; a value it cannot load would fail every later push, so
		// take it back.
		// ponytail: restored even when the push failed for another reason.
		if rerr := o.settings.Set(ctx, "proxy_custom", was); rerr != nil {
			return errors.Join(err, rerr)
		}
		return errors.Join(err, o.sync.Sync(ctx))
	}
	return err
}

// cleanPanelDomain is the host a panel_domain value stores: the form the
// proxy matches on, so a pasted URL, a host:port or a trailing dot cannot
// lock the panel out. A wildcard, localhost and an address are refused. The
// value the setting already has passes as it is (a save that changes nothing
// must not fail), and "" clears it.
func (o *Orchestrator) cleanPanelDomain(ctx context.Context, raw string) (string, error) {
	if raw = strings.TrimSpace(raw); raw == "" {
		return "", nil
	}
	if cur, _ := o.settings.Get(ctx, "panel_domain"); cur == raw {
		return raw, nil
	}
	host, err := installspec.CheckRoot(raw)
	if err != nil {
		return "", errs.Invalidf("panel_domain", "%s", err.Error())
	}
	if strings.HasPrefix(host, "*.") {
		return "", errs.Invalidf("panel_domain", "The panel needs one host, not a wildcard.")
	}
	return host, nil
}

// checkPanelAgainstRename refuses a panel_domain that a root_domain moved in
// the same save would turn into a tile's host: the panel vhost sorts first,
// so the tile would be dead.
func (o *Orchestrator) checkPanelAgainstRename(ctx context.Context, rawRoot, panel string) error {
	old, err := o.settings.Get(ctx, "root_domain")
	if err != nil {
		return err
	}
	oldBare, bare := strings.TrimPrefix(old, "*."), strings.TrimPrefix(installspec.CleanHost(rawRoot), "*.")
	if rawRoot == "" || bare == oldBare {
		return nil
	}
	inst, ok, err := o.instanceRow(ctx, oldBare)
	if err != nil || !ok {
		return err
	}
	p, err := o.planRename(ctx, inst, bare)
	if err != nil {
		return err
	}
	if slices.Contains(p.to, panel) || slices.Contains(p.rowTo, panel) {
		return errs.Conflictf("%s would be a tile domain after the root moves: the panel would shadow it.", panel)
	}
	return nil
}

// SettingDefaults is the server rung of the cascade.
func (o *Orchestrator) SettingDefaults(ctx context.Context) (settings.Settings, error) {
	return o.settings.Defaults(ctx)
}

// SetSettingDefaults merges into the server rung; every running tile
// redeploys (B34) and the proxy is re-pushed (protect).
func (o *Orchestrator) SetSettingDefaults(ctx context.Context, vals map[string]string) error {
	if err := o.checkValLimits(ctx, vals); err != nil {
		return err
	}
	if err := o.settings.SetDefaults(ctx, vals); err != nil {
		return err
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, og := range orgs {
		if err := o.redeployScope(ctx, ParamScope{Kind: "org", ID: og.ID}); err != nil {
			return err
		}
	}
	return o.sync.Sync(ctx)
}

// ---- self-upgrade ----

// Version is the running build.
func (o *Orchestrator) Version() string { return o.cfg.Version }

// CheckUpgrade asks GitHub for the latest release; newer says whether it
// is newer than this build (false on a dev build).
func (o *Orchestrator) CheckUpgrade(ctx context.Context) (latest string, newer bool, err error) {
	if latest, err = o.upgrade.Check(ctx); err != nil {
		return "", false, err
	}
	_, newer = o.upgrade.Available(latest)
	return latest, newer, nil
}

// Upgrade queues the self-upgrade to tag: pull, pre-upgrade archive, then
// the helper container swaps the panel.
func (o *Orchestrator) Upgrade(ctx context.Context, tag string) (Job, error) {
	if !o.upgrade.Upgradable() {
		return Job{}, errs.Conflictf("dev build, no upgrades")
	}
	return o.enqueue(ctx, kindUpgrade, upgradeJob{Tag: tag}, "panel")
}

// splitList reads a comma, space or newline separated setting.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
}
