package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
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
// there (B36); orgID "" is an unbound key, which only a stackr admin mints
// here (anyone gets one from stackr login, ExchangeCLICode). level is the optional
// ceiling (viewer|member|owner|admin) and stack an optional stack slug in the
// org; the ceiling may not exceed the minter's role. The token is shown once.
func (o *Orchestrator) MintKey(ctx context.Context, by *Principal, orgID, name, level, stack string) (string, APIKey, error) {
	// a stack key mints nothing; a capped key cannot mint past its own
	// ceiling (no level = no ceiling)
	if acc := by.Access; orgID == "" && (!acc.Admin || acc.KeyOrg != "") {
		return "", APIKey{}, errs.Refusedf("only a stackr admin can mint a key bound to no organization")
	} else if acc.KeyStack != "" {
		return "", APIKey{}, errs.Refusedf("a stack key cannot mint keys")
	} else if acc.KeyCapped {
		if l, ok := authz.ParseLevel(level); !ok || l > acc.KeyLevel {
			return "", APIKey{}, errs.Refusedf("a key cannot mint above its own level")
		}
	}
	return o.mintKey(ctx, by.User.ID, by.KeyID, orgID, name, level, stack)
}

// mintKey is the one place a key is made (the CLI browser exchange too); keyID
// is the key behind the request, "" for a session or an exchange. The key row
// and its audit event commit together.
func (o *Orchestrator) mintKey(ctx context.Context, userID, keyID, orgID, name, level, stack string) (string, APIKey, error) {
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
	stackID := ""
	if stack != "" {
		if orgID == "" {
			return "", APIKey{}, errs.Invalidf("stack", "a stack key needs an organization")
		}
		st, err := o.stacks.GetBySlug(ctx, orgID, stack)
		if errors.Is(err, errs.ErrNotFound) {
			return "", APIKey{}, errs.Invalidf("stack", "no stack %q in this organization", stack)
		}
		if err != nil {
			return "", APIKey{}, err
		}
		stackID = st.ID
	}
	var tok string
	var k APIKey
	err = o.store.Tx(ctx, func(tx store.Tx) (err error) {
		if tok, k, err = user.New(tx.Users, tx.Sessions, tx.APIKeys).MintKey(ctx, u, orgID, role, name, level, stackID); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.KeyMint, ActorID: userID, KeyID: keyID, TargetKind: "api_key", TargetID: k.ID,
			Detail: fmt.Sprintf("name=%s org=%s level=%s stack=%s", k.Name, orgID, level, stackID),
		})
	})
	if err != nil {
		return "", APIKey{}, err
	}
	k.Stack = stack
	return tok, k, nil
}

// Keys lists the user's keys; Stack is the slug of the stack a key is limited to.
func (o *Orchestrator) Keys(ctx context.Context, userID string) ([]APIKey, error) {
	ks, err := o.users.Keys(ctx, userID)
	for i := range ks {
		if ks[i].StackID == nil {
			continue
		}
		if st, err := o.stacks.Get(ctx, *ks[i].StackID); err == nil {
			ks[i].Stack = st.Slug
		}
	}
	return ks, err
}

// RevokeKey deletes one of the caller's keys; a capped or stack key may
// revoke only itself.
func (o *Orchestrator) RevokeKey(ctx context.Context, by *Principal, keyID string) error {
	if (by.Access.KeyCapped || by.Access.KeyStack != "") && keyID != by.KeyID {
		return errs.Refusedf("a limited key can revoke only itself")
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := user.New(tx.Users, tx.Sessions, tx.APIKeys).RevokeKey(ctx, by.User.ID, keyID); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.KeyRevoke, ActorID: by.User.ID, KeyID: by.KeyID, TargetKind: "api_key", TargetID: keyID,
		})
	})
}

// ---- settings: the one catalogue ----

// Settings is the catalogue every surface enumerates.
func (o *Orchestrator) Settings() []Knob { return settings.Catalogue }

// Setting is a flat knob's effective value; the alert webhook's is masked as
// the Alerts tab shows it.
func (o *Orchestrator) Setting(ctx context.Context, key string) (string, error) {
	v, err := o.settings.Get(ctx, key)
	if key == alertURLKey {
		v = settings.MaskURL(v)
	}
	return v, err
}

// scheduleKnobs are the settings the cron table is built from.
var scheduleKnobs = map[string]bool{
	"panel_backup_enabled":      true,
	"panel_backup_schedule":     true,
	"cleanup_schedule":          true,
	"cleanup_enabled":           true,
	"orphans_enabled":           true,
	"orphans_schedule":          true,
	"registry_cleanup_schedule": true,
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
		if key == "acme_email" || key == "dns_provider" || key == "alert_webhook_url" {
			// what the server file's Parse refuses, so export then plan reads clean
			if _, err := serverconfig.CheckSetting(key, raw); err != nil {
				return errs.Invalidf(key, "%s", err.Error())
			}
		}
		// The masked form reads back as a valid URL; saving it would drop the
		// real webhook.
		if key == "alert_webhook_url" && strings.HasSuffix(raw, "/…") {
			return errs.Invalidf(key, "That is the masked URL. Paste the full webhook URL.")
		}
		if key == "trusted_proxies" {
			for _, r := range splitList(raw) {
				if strings.EqualFold(r, domain.CloudflareKeyword) {
					continue
				}
				if _, err := netip.ParsePrefix(r); err != nil {
					if _, err := netip.ParseAddr(r); err != nil {
						return errs.Invalidf("trusted_proxies", "%q is not an IP, a CIDR or cloudflare.", r)
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
	panelWas, _ := o.settings.Get(ctx, "panel_domain")
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
	if slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, "observe_") }) {
		if k, err := o.observeKnobs(ctx); err == nil {
			o.observe.on.Store(k.on)
		}
		o.startSyncObserve() // a first pull takes a while; the tab shows how it went
	}
	err := o.sync.Sync(ctx)
	if p, ok := vals["panel_domain"]; ok && p != panelWas {
		// GitHub Apps keep the webhook URL they were made with; a failure is
		// logged per App and does not fail the save. Runs even after a failed
		// push: the row is written, and a re-save of it would not get here.
		_ = o.SyncGitHubHooks(ctx)
	}
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

// keyUpgradeLatest is the release the last update check found (a settings
// row, not a knob): the Update tab shows it after a restart.
const keyUpgradeLatest = "upgrade_latest"

// CheckUpgrade asks GitHub for the latest release and remembers it; newer
// says whether it is newer than this build (false on a dev build).
func (o *Orchestrator) CheckUpgrade(ctx context.Context) (latest string, newer bool, err error) {
	if latest, err = o.upgrade.Check(ctx); err != nil {
		return "", false, err
	}
	if err := o.store.Settings.Set(ctx, keyUpgradeLatest, latest); err != nil {
		slog.Warn("update check: not remembered", "err", err)
	}
	_, newer = o.upgrade.Available(latest)
	return latest, newer, nil
}

// LastUpgradeCheck is the last check's answer, compared with this build;
// ok is false when none ran. A dev build needs none: its answer is "".
func (o *Orchestrator) LastUpgradeCheck(ctx context.Context) (latest string, newer, ok bool) {
	if !o.upgrade.Upgradable() {
		return "", false, true
	}
	latest, ok, _ = o.store.Settings.Get(ctx, keyUpgradeLatest)
	_, newer = o.upgrade.Available(latest)
	return latest, newer, ok
}

// Upgrade queues the self-upgrade to tag: pull, pre-upgrade archive, then
// the helper container swaps the panel.
func (o *Orchestrator) Upgrade(ctx context.Context, tag string) (Job, error) {
	if !o.upgrade.Upgradable() {
		return Job{}, errs.Conflictf("A dev build cannot upgrade itself. Run stackr-install from the release to move this box onto it.")
	}
	// The new panel rolls the workers onto its build once it has published
	// its image (rolloutAfterBoot); a stop from an earlier rollout is over.
	if err := o.store.Settings.Delete(ctx, keyRolloutStopped); err != nil {
		return Job{}, err
	}
	if err := o.setRolloutTarget(ctx, tag); err != nil {
		return Job{}, err
	}
	return o.enqueue(ctx, kindUpgrade, upgradeJob{Tag: tag}, "panel")
}

// upgradeSwapWindow is how long after launching its successor a panel still
// says it is being replaced.
// ponytail: a fixed window, as the helper's gate reports nothing back here;
// a helper that rolls back leaves the old panel saying so until it passes.
const upgradeSwapWindow = 10 * time.Minute

// UpgradeUnderway: an upgrade job is queued or running, or this panel launched
// the helper that replaces it moments ago and is still serving. The Update tab
// keeps itself live through the restart on it, and offers no buttons.
func (o *Orchestrator) UpgradeUnderway(ctx context.Context) bool {
	if at := o.upgradeLaunched.Load(); at != 0 && time.Since(time.Unix(0, at)) < upgradeSwapWindow {
		return true
	}
	js, err := o.store.Jobs.ListByState(ctx, "queued", "running")
	return err == nil && slices.ContainsFunc(js, func(j store.Job) bool { return j.Kind == string(kindUpgrade) })
}

// splitList reads a comma, space or newline separated setting.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
}
