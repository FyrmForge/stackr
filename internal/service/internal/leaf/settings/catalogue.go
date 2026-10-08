// Package settings is the one catalogue of knobs every surface enumerates,
// the cascade (server → org → stack → env → tile) and the install-wide rows
// of the settings table. A knob's Key is the only spelling of its name.
package settings

import "slices"

type Type string

const (
	TInt   Type = "int"
	TFloat Type = "float"
	TBool  Type = "bool"
	TStr   Type = "string"
)

// Scope is which rungs may set a knob. Flat knobs have no rungs: one row in
// the settings table, install-wide.
type Scope uint8

const (
	Server Scope = 1 << iota
	Org
	Stack
	Env
	Tile
	Flat

	Below = Org | Stack | Env
	All   = Server | Below
)

// String names a single scope for refusals.
func (s Scope) String() string {
	switch s {
	case Server:
		return "server"
	case Org:
		return "org"
	case Stack:
		return "stack"
	case Env:
		return "environment"
	case Tile:
		return "tile"
	case Flat:
		return "install"
	}
	return "scope"
}

type Knob struct {
	Key       string `json:"key"`
	Type      Type   `json:"type"`
	Default   string `json:"default"`
	Scopes    Scope  `json:"scopes"`
	AllowZero bool   `json:"allow_zero"` // an explicit 0 is a real value, not "clear"
	ReadOnly  bool   `json:"read_only"`  // the installer sets it; Set refuses
	// ConfigOnly marks the server file's own binding (server_config_*): the
	// Config tab and bind verb own them, so the Settings tab and the server
	// file's settings: block skip them.
	ConfigOnly bool   `json:"config_only"`
	Desc       string `json:"desc"`
}

// DefaultsKey is the settings-table row holding the server rung of the
// cascade, a Settings blob like the org/stack/env columns.
const DefaultsKey = "defaults"

// Catalogue is enumerated, never indexed by a literal at a call site.
// ponytail: the "later" knobs (cron, metrics, multi-node) are left out until
// their feature lands; add each here with its Settings field.
var Catalogue = []Knob{
	// The cascade.
	{
		Key:       "cpu_limit",
		Type:      TFloat,
		Default:   "0",
		Scopes:    All | Tile,
		AllowZero: true,
		Desc:      "CPU cores; 0 is unlimited",
	},
	{
		Key:       "mem_limit_mb",
		Type:      TInt,
		Default:   "0",
		Scopes:    All | Tile,
		AllowZero: true,
		Desc:      "memory cap in MB; 0 is unlimited",
	},
	{
		Key:     "protect",
		Type:    TBool,
		Default: "false",
		Scopes:  All,
		Desc:    "basic auth in front of every URL below this level",
	},
	{
		Key:    "protect_user",
		Type:   TStr,
		Scopes: All,
		Desc:   "basic auth user",
	},
	{
		Key:    "protect_password",
		Type:   TStr,
		Scopes: All,
		Desc:   "plain text or a ${{ }} ref",
	},

	// Install-wide rows.
	{
		Key:     "workers",
		Type:    TInt,
		Default: "2",
		Scopes:  Flat,
		Desc:    "jobs that run at once",
	},
	{
		Key:       "image_check_interval",
		Type:      TInt,
		Default:   "60",
		Scopes:    Flat,
		AllowZero: true,
		Desc:      "minutes between image-watch checks; 0 is off",
	},
	{
		Key:     "orphan_retention_days",
		Type:    TInt,
		Default: "30",
		Scopes:  Flat,
		Desc:    "days an orphaned volume is kept before its last backup and delete",
	},
	{
		Key:     "backup_run_concurrency",
		Type:    TInt,
		Default: "2",
		Scopes:  Flat,
		Desc:    "backups that run at once",
	},
	{
		Key:     "backup_restore_concurrency",
		Type:    TInt,
		Default: "1",
		Scopes:  Flat,
		Desc:    "restores that run at once",
	},
	{
		Key:    "panel_domain",
		Type:   TStr,
		Scopes: Flat,
		Desc:   "the domain the panel answers on",
	},
	{
		Key:    "root_domain",
		Type:   TStr,
		Scopes: Flat,
		Desc:   "the root domain; tiles get names under it",
	},
	{
		Key:    "acme_email",
		Type:   TStr,
		Scopes: Flat,
		Desc:   "contact address for certificates",
	},
	{
		Key:    "trusted_proxies",
		Type:   TStr,
		Scopes: Flat,
		Desc:   "CIDRs whose X-Forwarded-For is trusted",
	},
	{
		Key:    "dns_provider",
		Type:   TStr,
		Scopes: Flat,
		Desc:   "DNS-01 provider for the wildcard certificate",
	},
	{
		Key:    "proxy_custom",
		Type:   TStr,
		Scopes: Flat,
		Desc:   "admin-only extra Caddy config",
	},
	{
		Key:     "cleanup_enabled",
		Type:    TBool,
		Default: "false",
		Scopes:  Flat,
		Desc:    "nightly image and build cache sweep",
	},
	{
		Key:     "cleanup_schedule",
		Type:    TStr,
		Default: "30 4 * * *",
		Scopes:  Flat,
		Desc:    "when the sweep runs, a cron line; empty is the default time (cleanup_enabled is the switch)",
	},
	{
		Key:     "orphans_enabled",
		Type:    TBool,
		Default: "true",
		Scopes:  Flat,
		Desc:    "back up and delete orphaned volumes past retention",
	},
	{
		Key:     "orphans_schedule",
		Type:    TStr,
		Default: "@daily",
		Scopes:  Flat,
		Desc:    "when orphaned volumes past retention are backed up and deleted, a cron line; empty is the default time",
	},
	{
		Key:     "panel_backup_enabled",
		Type:    TBool,
		Default: "true",
		Scopes:  Flat,
		Desc:    "the panel archives itself on its schedule",
	},
	{
		Key:     "panel_backup_schedule",
		Type:    TStr,
		Default: "0 3 * * *",
		Scopes:  Flat,
		Desc:    "when the panel archives itself, a cron line; empty is the default time",
	},
	{
		Key:     "panel_backup_keep",
		Type:    TInt,
		Default: "14",
		Scopes:  Flat,
		Desc:    "scheduled panel archives kept at the destination",
	},
	{
		Key:    "panel_backup_dest",
		Type:   TStr,
		Scopes: Flat,
		Desc:   "global backup destination id for panel archives; empty is local",
	},

	// The server file's binding (stackr-server.yml); written by
	// BindServerConfig, shown on the admin Config tab.
	{
		Key:        "server_config_connector",
		Type:       TStr,
		Scopes:     Flat,
		ConfigOnly: true,
		Desc:       "id of the server connector the server file is read through",
	},
	{
		Key:        "server_config_repo",
		Type:       TStr,
		Scopes:     Flat,
		ConfigOnly: true,
		Desc:       "the repo that holds stackr-server.yml; empty is unbound",
	},
	{
		Key:        "server_config_branch",
		Type:       TStr,
		Scopes:     Flat,
		ConfigOnly: true,
		Desc:       "the branch of the server file; empty is the repo's default",
	},
	{
		Key:        "server_config_path",
		Type:       TStr,
		Scopes:     Flat,
		ConfigOnly: true,
		Desc:       "path of the server file in the repo; empty is stackr-server.yml",
	},
	{
		Key:        "server_config_auto",
		Type:       TBool,
		Default:    "false",
		Scopes:     Flat,
		ConfigOnly: true,
		Desc:       "apply a server plan with no impact lines and no removals by itself",
	},
}

// Default is a knob's catalogue default, "" for an unknown key.
func Default(key string) string {
	k, _ := lookup(key)
	return k.Default
}

// cronKnobs hold a five-field cron line (CRON_TZ= allowed). An empty write
// removes the row, so the default time applies; the on/off switches are
// separate knobs.
var cronKnobs = map[string]bool{
	"panel_backup_schedule": true,
	"cleanup_schedule":      true,
	"orphans_schedule":      true,
}

// IsCron says whether key holds a cron line.
func IsCron(key string) bool { return cronKnobs[key] }

func lookup(key string) (Knob, bool) {
	i := slices.IndexFunc(Catalogue, func(k Knob) bool { return k.Key == key })
	if i < 0 {
		return Knob{}, false
	}
	return Catalogue[i], true
}
