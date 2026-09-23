package settings

// The key space of the `settings` table: one flat string->string map for the
// knobs that belong to the installation rather than to any org, stack or
// environment.
//
// Note this is a different table from the one the rest of this package is
// about. The cascade (Parse/Merge/Check, and service.SettingsService on top
// of it) is the `settings` COLUMN on server, org, stack and environment rows
// — a typed blob that inherits down four levels. What is below is the
// untyped k/v table behind repo.Store's GetSetting/SetSetting, which has no
// levels and no schema.
//
// They live in one file because the confusion between them is the reason the
// keys were spelled in three packages at once: service/proxy held seven of
// them, service/admin three, service/imagewatch one, and cmd/stackrd and
// infra/githubapp typed theirs inline. A key typed by hand in a second place
// reads back "" and the knob is silently off — which is how `dns_provider`
// came to be read by both infra/proxy and config/stackconf with nothing
// saying they meant the same thing.
const (
	// Proxy: the escape hatches. In the DB rather than on disk so a wiped
	// data dir heals from the next Resync.
	KeyCustomDynamic  = "proxy_custom_dynamic"
	KeyStaticOverride = "traefik_static_override"
	KeyTrustedProxies = "trusted_proxies"
	KeyTrustCF        = "trust_cloudflare"
	KeyCFCIDRs        = "cloudflare_cidrs"
	KeyDNSProvider    = "dns_provider"
	KeyDNSEnv         = "dns_env"

	// Panel upgrades.
	KeyUpgradeLatest    = "upgrade_latest"
	KeyUpgradeCheckedAt = "upgrade_checked_at"
	KeyUpgradeArchive   = "upgrade_archive"

	// The image watcher: check cadence in minutes ("" = 5, "0" = the watch
	// off) and the RFC3339 stamp of the last sweep. The janitor ticks every
	// minute, which is what makes the cadence runtime-changeable.
	KeyImageInterval = "image_check_interval"
	KeyImageLast     = "image_check_last"

	// Whether the nightly docker/registry sweep runs.
	KeyCleanupEnabled = "cleanup_enabled"

	// Marks the installer's answers as copied in. Once only: after the first
	// boot the panel owns them, and a value the operator removed must not
	// come back on the next restart.
	KeyInstallSeeded = "install_seeded"
)

// PRPlanKey holds the rendered plan-preview markdown for one pull request, so
// the comment can be rewritten in place rather than appended to.
func PRPlanKey(stackID, prNum string) string { return "prplan." + stackID + "." + prNum }
