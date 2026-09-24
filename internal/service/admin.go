package service

import (
	"context"
	"net/netip"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
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

// SetSetting writes a flat knob (a typo is refused, B24). The proxy is
// re-pushed, since several flat knobs live in its config; workers and the
// watch interval are re-read live.
func (o *Orchestrator) SetSetting(ctx context.Context, key, raw string) error {
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
	if err := o.settings.Set(ctx, key, raw); err != nil {
		return err
	}
	return o.sync.Sync(ctx)
}

// SettingDefaults is the server rung of the cascade.
func (o *Orchestrator) SettingDefaults(ctx context.Context) (settings.Settings, error) {
	return o.settings.Defaults(ctx)
}

// SetSettingDefaults merges into the server rung; every running tile
// redeploys (B34) and the proxy is re-pushed (protect).
func (o *Orchestrator) SetSettingDefaults(ctx context.Context, vals map[string]string) error {
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
