// Package stackconf parses stackr-compose.yml, declarative stack config,
// and resolves it into per-environment tile definitions. Parsing and merging
// are pure; fetching files and diffing against the database live elsewhere
// (the fetcher is injected, the diff engine is in plan.go).
//
// Merge model: tiles are kept as raw YAML maps until after include- and
// environment-overlay merging, then decoded into typed TileConf. That is what
// makes sparse overlays trivial, an overlay only overrides the keys it
// mentions.
package stackconf

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/config/runpolicy"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// DefaultPath is where the config file lives unless the stack says otherwise.
const DefaultPath = "stackr-compose.yml"

// File is the parsed (but not yet resolved) config file.
type File struct {
	Version int      `yaml:"version"`
	Stack   string   `yaml:"stack"`
	PREnvs  *PREnvs  `yaml:"pr_envs,omitempty"`
	Include []string `yaml:"include,omitempty"`
	// Secrets names the values this stack needs but does not carry, never
	// values, the file is in git. Declared once at stack level, valued per
	// env (unless env_versions: false). A bare key keeps today's semantics:
	// set out of band, standing plan warning while unset.
	Secrets SecretsNode `yaml:"secrets,omitempty"`
	// Vars are stack-wide plain values, read as ${{ stack.vars.NAME }}. Unlike
	// secrets: the value lives in the file, so this is for things that are
	// configuration, not credentials. An environment's own vars: shadows one.
	Vars map[string]string `yaml:"vars,omitempty"`
	// Defaults is this stack's level of the defaults cascade (server -> org ->
	// stack -> env -> tile). Same keys at every level; see
	// internal/stackrd/config/settings.
	Defaults DefaultsConf `yaml:"defaults,omitempty"`
	// Moved declares renames: map keys are identity to the differ, so without
	// it a renamed key plans a delete and a create, which for a tile with a
	// volume destroys data. See moved.go.
	Moved        []MovedEntry `yaml:"moved,omitempty"`
	Environments EnvsNode     `yaml:"environments"`
	// Shared is the stack-scoped tiles, managed instances shared by every
	// env. Scope is structural: an entry here is stack-scoped, a tile under an
	// environment is env-scoped. The instance row physically lives in the
	// default (first) environment.
	Shared map[string]RawMap `yaml:"shared,omitempty"`
	// Base is tiles defined once that land in EVERY static env. An env entry
	// with the same name is a sparse overlay (mergeMaps) or an exclusion
	// (false). Unlike shared:, each env gets its own copy, base slices are
	// per-env logical dbs.
	Base BaseConf `yaml:"base,omitempty"`
	// Domains are the stack's own domain resources, the bases its tiles
	// generate per-environment hostnames under (envops.AutoHost). Declaring
	// them here is what lets a managed stack own its own hostnames instead of
	// borrowing its org's.
	Domains []DomainResConf `yaml:"domains,omitempty"`
	// UIEdits: what the panel does with an edit to a field this file owns,
	// block (default) or stage. Blank inherits the org file's defaults:.
	UIEdits string `yaml:"ui_edits,omitempty"`
	// Proxy carries raw traefik middlewares this stack's domains, and other
	// stacks' in the org, can name (middlewares.go).
	Proxy ProxyConf `yaml:"proxy,omitempty"`
}

// DomainResConf is one declared domain resource. Hosts are unique across the
// whole server, so a clash is a plan error rather than an apply error.
type DomainResConf struct {
	Host string `yaml:"host"`
	// ACMEEmail is the Let's Encrypt account certificates under this resource
	// are issued on. Blank uses the instance's.
	ACMEEmail string `yaml:"acme_email,omitempty"`
	// IncludeEnvOnDefault keeps the default env's slug in generated names
	// (api.prod.stack.org instead of api.stack.org).
	IncludeEnvOnDefault bool `yaml:"include_env_on_default"`
}

// ValidateDomains rejects a blank or duplicated host before anything is planned.
func ValidateDomains(ds []DomainResConf) error {
	seen := map[string]bool{}
	for _, d := range ds {
		if strings.TrimSpace(d.Host) == "" {
			return fmt.Errorf("domains: host required")
		}
		if seen[d.Host] {
			return fmt.Errorf("domains: %s declared twice", d.Host)
		}
		seen[d.Host] = true
	}
	return nil
}

// ValidateUIEdits accepts the two modes, or blank for "inherit / default".
func ValidateUIEdits(v string) error {
	switch v {
	case "", repo.UIEditsBlock, repo.UIEditsStage:
		return nil
	}
	return fmt.Errorf("ui_edits: %q is not block or stage", v)
}

// BaseConf is the base: section.
type BaseConf struct {
	Tiles map[string]RawMap `yaml:"tiles"`
}

// PREnvs mirrors envops.PRConfig plus the PR env's tile template. A PR env is
// base + Tiles, built purely from the file, never cloned from a live env, so
// panel drift can't leak into previews.
type PREnvs struct {
	// nil = inherit the panel setting. A plain bool made `pr_envs: {tiles: ...}`
	// silently disable PR envs, since the file wins over the panel.
	Enabled *bool `yaml:"enabled"`
	Comment *bool `yaml:"comment"`
	Status  *bool `yaml:"status"`
	// Against limits which PR target branches spawn an env (empty = any).
	Against []string `yaml:"against"`
	// Tiles overlays the base tiles exactly like an environment's entries do
	// (sparse patch or false = exclusion).
	Tiles map[string]TileNode `yaml:"tiles"`
}

// RawMap is a tile body kept unmerged. Decoding to TileConf happens after
// all merging (includes, overlays) is done.
type RawMap map[string]any

// EnvsNode accepts both forms:
//
//	environments: [production, staging]
//	environments: {production: {...}, staging: {...}}
//
// Order is preserved, the first environment is the default.
type EnvsNode struct {
	Order []string
	Envs  map[string]EnvConf
}

// EnvConf is one environment: base overlays plus its own tiles.
type EnvConf struct {
	Protected bool `yaml:"protected,omitempty"`
	// Color is how the panel draws this environment: a palette name
	// (violet, teal, amber, rose, sky, lime) or #rrggbb. See envcolor.
	Color string `yaml:"color,omitempty"`
	// Secrets an environment needs on top of the stack-level ones.
	Secrets []string `yaml:"secrets,omitempty"`
	// Defaults is this environment's level of the defaults cascade.
	Defaults DefaultsConf `yaml:"defaults,omitempty"`
	// ApplyPolicy is what a plan touching this environment does: "auto"
	// applies on its own, "manual" waits for a person. Empty is the per-rung
	// default (manual for the first environment, auto above it).
	ApplyPolicy string `yaml:"apply_policy,omitempty"`
	// Vars this environment sets, shadowing the stack-level vars: at
	// resolution time. Needs the map form of environments:, a list of names
	// has nowhere to carry it.
	Vars  map[string]string   `yaml:"vars,omitempty"`
	Tiles map[string]TileNode `yaml:"tiles,omitempty"`
}

// DefaultsConf is one level of the defaults cascade as the config file writes
// it. Every field is a pointer: nil is "say nothing here", which is what lets a
// level inherit rather than reset the level above to a zero value.
//
// The same struct at org, stack and env level: one spelling everywhere, so a
// knob cannot mean one thing in stackr-org.yml and another in
// stackr-compose.yml.
type DefaultsConf struct {
	CronTimeoutMin       *int     `yaml:"cron_timeout_min" json:"cron_timeout_min,omitempty"`
	CPULimit             *float64 `yaml:"cpu_limit" json:"cpu_limit,omitempty"`
	MemLimitMB           *int     `yaml:"mem_limit_mb" json:"mem_limit_mb,omitempty"`
	RunRetentionDays     *int     `yaml:"run_retention_days" json:"run_retention_days,omitempty"`
	MetricRetentionHours *int     `yaml:"metric_retention_hours" json:"metric_retention_hours,omitempty"`
	ProtectAutoDomains   *bool    `yaml:"protect_auto_domains" json:"protect_auto_domains,omitempty"`
	NodeGroup            *string  `yaml:"node_group" json:"node_group,omitempty"`
}

// SettingsJSON renders the level for storage, the shape
// internal/stackrd/config/settings reads. build_node is deliberately not a
// config key: it is instance-wide and read only at the server level.
func (d DefaultsConf) SettingsJSON() string {
	b, err := json.Marshal(d)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Empty reports whether the file said nothing at this level, which is not the
// same as saying "inherit everything": a level that declares nothing leaves
// whatever the panel set alone.
func (d DefaultsConf) Empty() bool { return d.SettingsJSON() == "{}" }

// BackupConf is a tile's backup schedule. The destination is a reference and
// never a literal: a destination carries the bucket credentials, and the file
// is in git.
//
// Kind is not declared. A managed database is dumped with its engine's own
// tool; a tile with a volume gets a tar of the volume. There is no third
// answer, and asking the file to repeat it invites the two disagreeing.
type BackupConf struct {
	// Dest is ${{ org.backups.NAME }} or ${{ stackr.backups.NAME }}.
	Dest     string `yaml:"dest" json:"dest"`
	Schedule string `yaml:"schedule" json:"schedule"`
	// TZ names the zone the schedule is read in; the server's when absent.
	TZ string `yaml:"tz" json:"tz,omitempty"`
	// Keep is how many archives survive a prune; 0 keeps every one.
	Keep int `yaml:"keep" json:"keep,omitempty"`
	// Mode is what happens to the container while a volume is tarred:
	// pause (default), stop, or live. Volume tiles only; on a dump it is a
	// plan error rather than a silently ignored key.
	Mode string `yaml:"mode" json:"mode,omitempty"`
	// Enabled defaults to true; `enabled: false` keeps the schedule declared
	// and stops it firing.
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
}

// On reports whether the schedule should fire, with the default applied.
func (b BackupConf) On() bool { return b.Enabled == nil || *b.Enabled }

// SecretConf is one declared secret's options. The zero value is a bare
// declaration: set out of band, warn while unset.
type SecretConf struct {
	// Default: "" = set out of band; "generated" = minted on the first apply
	// that finds it unset, then stable forever, later applies never touch
	// it, and changing the generation knobs never rotates an existing value.
	Default        string `yaml:"default" json:"default,omitempty"`
	Length         int    `yaml:"length" json:"length,omitempty"` // generated only; 0 = 32
	IncludeNumbers bool   `yaml:"include_numbers" json:"include_numbers,omitempty"`
	IncludeSymbols bool   `yaml:"include_symbols" json:"include_symbols,omitempty"`
	// Required: apply refuses while unset (instead of warning and deploying
	// services that crash without the value).
	Required bool `yaml:"required" json:"required,omitempty"`
	// EnvVersions (default true): each env holds its own value; generation
	// mints one per env. false = one stack-wide value, env overrides refused.
	EnvVersions *bool `yaml:"env_versions" json:"env_versions,omitempty"`
}

// PerEnv is EnvVersions with the default applied.
func (sc SecretConf) PerEnv() bool { return sc.EnvVersions == nil || *sc.EnvVersions }

// GenLength is the generation length with the default applied.
func (sc SecretConf) GenLength() int {
	if sc.Length > 0 {
		return sc.Length
	}
	return 32
}

// SecretsNode is the secrets: map. A key with a null body is a bare
// declaration.
type SecretsNode map[string]SecretConf

func (sn *SecretsNode) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("secrets must be a map of NAME to options (a bare NAME: declares it)")
	}
	*sn = SecretsNode{}
	for i := 0; i < len(n.Content); i += 2 {
		name := n.Content[i].Value
		var sc SecretConf
		if n.Content[i+1].Tag != "!!null" {
			if err := strictNode(n.Content[i+1], &sc); err != nil {
				return fmt.Errorf("secret %s: %w", name, err)
			}
		}
		(*sn)[name] = sc
	}
	return nil
}

// TileNode is an entry under an environment's tiles: either an exclusion
// (false/null, meaningful in pr_envs.overrides) or a tile body.
type TileNode struct {
	Excluded bool
	Raw      RawMap
}

func (t *TileNode) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var b bool
		if err := n.Decode(&b); err == nil && !b {
			t.Excluded = true
			return nil
		}
		if n.Tag == "!!null" {
			t.Excluded = true
			return nil
		}
		return fmt.Errorf("tile entry must be a map or false, got %q", n.Value)
	case yaml.MappingNode:
		return n.Decode(&t.Raw)
	}
	return fmt.Errorf("tile entry must be a map or false")
}

func (e *EnvsNode) UnmarshalYAML(n *yaml.Node) error {
	e.Envs = map[string]EnvConf{}
	switch n.Kind {
	case yaml.SequenceNode:
		var names []string
		if err := n.Decode(&names); err != nil {
			return err
		}
		e.Order = names
		for _, name := range names {
			e.Envs[name] = EnvConf{}
		}
		return nil
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			name := n.Content[i].Value
			var ec EnvConf
			if n.Content[i+1].Tag != "!!null" {
				if err := strictNode(n.Content[i+1], &ec); err != nil {
					return fmt.Errorf("environment %s: %w", name, err)
				}
			}
			e.Order = append(e.Order, name)
			e.Envs[name] = ec
		}
		return nil
	}
	return fmt.Errorf("environments must be a list or a map")
}

// TileConf is one fully-merged tile definition. The json tags mirror the yaml
// ones so UI-staging patches (stored as JSON) merge onto a serialized tile
// under the same field names, see internal/stackrd/config/stackconf/staging.go.
type TileConf struct {
	Type   string     `yaml:"type" json:"type"` // service (default) | cron | function | managed | volume; from: implies slice
	Build  *BuildConf `yaml:"build" json:"build,omitempty"`
	Branch string     `yaml:"branch" json:"branch,omitempty"`
	Image  string     `yaml:"image" json:"image,omitempty"`
	// Git source. Config-managed stacks leave these empty and inherit the
	// bound repo/connector (via DiffOpts); UI-managed tiles carry them
	// explicitly so a UI git service round-trips through create + diff.
	GitURL          string       `yaml:"git_url" json:"git_url,omitempty"`
	Connector       string       `yaml:"connector" json:"connector,omitempty"`
	Port            int          `yaml:"port" json:"port,omitempty"`
	Domains         []DomainConf `yaml:"domains" json:"domains,omitempty"`
	Env             EnvMap       `yaml:"env" json:"env,omitempty"`
	Limits          *LimitsConf  `yaml:"limits" json:"limits,omitempty"`
	Healthcheck     string       `yaml:"healthcheck" json:"healthcheck,omitempty"`
	SecurityHeaders bool         `yaml:"security_headers" json:"security_headers,omitempty"`
	Volumes         []string     `yaml:"volumes" json:"volumes,omitempty"`
	WatchPaths      []string     `yaml:"watch_paths" json:"watch_paths,omitempty"` // regex per line, "!" prefix = ignore
	// Backup is this tile's backup schedule, one per tile. Nil means the file
	// says nothing; on a config-owned tile that is a delete, same as any other
	// key it stops declaring.
	Backup *BackupConf `yaml:"backup" json:"backup,omitempty"`
	// UpdatePolicy: registry-watcher behavior for image-source tiles,
	// "off" (default), "notify" (badge + notification) or "auto" (redeploy).
	UpdatePolicy string `yaml:"update_policy" json:"update_policy,omitempty"`
	// WaitForCI parks push auto-deploys until the commit's checks pass
	// (git-source tiles).
	WaitForCI bool `yaml:"wait_for_ci" json:"wait_for_ci,omitempty"`
	// Service build/routing extras editable from the settings form. These are
	// modeled so UI settings edits stage through the config engine.
	BuildArgs       string `yaml:"build_args" json:"build_args,omitempty"`
	PublishedPorts  string `yaml:"published_ports" json:"published_ports,omitempty"`
	TraefikOverride string `yaml:"traefik_override" json:"traefik_override,omitempty"`
	BasicAuthUser   string `yaml:"basic_auth_user" json:"basic_auth_user,omitempty"`
	BasicAuthHash   string `yaml:"basic_auth_hash" json:"basic_auth_hash,omitempty"` // bcrypt; staged from the form pre-hashed
	// Container runtime fields (services; shm_size_mb also applies to managed
	// instances). Restart is "" or "always" (restart on any exit),
	// "on-failure", or "no" (runtime.NormalizeRestart); Devices lines
	// are "host[:container[:perms]]"; User is docker's --user.
	User       string   `yaml:"user" json:"user,omitempty"`
	ShmSizeMB  int      `yaml:"shm_size_mb" json:"shm_size_mb,omitempty"`
	Privileged bool     `yaml:"privileged" json:"privileged,omitempty"`
	Devices    []string `yaml:"devices" json:"devices,omitempty"`
	Restart    string   `yaml:"restart" json:"restart,omitempty"`
	// Placement (docs/plans/31-node-agent-open-questions.md, step 8).
	// Replicas above 1 on a tile holding a volume is a config error, not
	// something quietly forced back to 1: one mounter per volume, always.
	// NodeGroup constrains the tile to nodes labelled stackr.group=<x>;
	// empty inherits from the env, stack and org.
	Replicas  int    `yaml:"replicas" json:"replicas,omitempty"`
	NodeGroup string `yaml:"node_group" json:"node_group,omitempty"`
	// Docker-native healthcheck knobs for Healthcheck, in seconds (0 = default).
	HealthInterval    int `yaml:"healthcheck_interval" json:"healthcheck_interval,omitempty"`
	HealthTimeout     int `yaml:"healthcheck_timeout" json:"healthcheck_timeout,omitempty"`
	HealthRetries     int `yaml:"healthcheck_retries" json:"healthcheck_retries,omitempty"`
	HealthStartPeriod int `yaml:"healthcheck_start_period" json:"healthcheck_start_period,omitempty"`
	// function: run again whenever its own deploy finishes (manual Run now
	// always works).
	RunOnDeploy bool `yaml:"run_on_deploy" json:"run_on_deploy,omitempty"`
	// DependsOn: startup-order dependencies, "slug" or "slug:condition"
	// (started, the default | healthy | completed). Honored on bulk starts
	// (config apply); a manual single-tile deploy does not wait.
	DependsOn []string `yaml:"depends_on" json:"depends_on,omitempty"`
	// Files ships repo files into the container: map or line form, fetched
	// from the tile's git coordinates at deploy, bind-mounted read-only;
	// :template runs the varref resolver over the file bytes.
	Files FileList `yaml:"files" json:"files,omitempty"`
	// Storage attaches declared storage sub-paths (§2.7):
	// "storage-slug/path-name:/mount[:ro]" per entry.
	Storage []string `yaml:"storage" json:"storage,omitempty"`
	// cron (command is shared: a cron's one-shot line or a service's CMD override)
	Schedule       string `yaml:"schedule" json:"schedule,omitempty"`
	Command        string `yaml:"command" json:"command,omitempty"`
	TimeoutMinutes int    `yaml:"timeout_minutes" json:"timeout_minutes,omitempty"`
	AllowOverlap   bool   `yaml:"allow_overlap" json:"allow_overlap,omitempty"`
	// db
	Engine string `yaml:"engine" json:"engine,omitempty"`
	// ExternalPort publishes the instance on the host (0 = internal only).
	ExternalPort int `yaml:"external_port" json:"external_port,omitempty"`
	// Scope is structural in the file (shared: = stack, under an env = env),
	// so it is not a YAML key, resolve() sets it. It stays on the JSON side
	// because UI-staging patches and the serializer still carry it explicitly
	// (org included: org-scoped instances are panel/CLI-owned and invisible to
	// Snapshot until org-level config exists).
	Scope string `yaml:"-" json:"scope,omitempty"`
	// volume
	Attach string `yaml:"attach" json:"attach,omitempty"` // tile slug mounting this volume ("" = detached)
	Path   string `yaml:"path" json:"path,omitempty"`     // container mount path
	// slice: a logical db / bucket cut from a managed instance. The config
	// key is the reference slug (${{ tile.<key>.<OUTPUT> }}); From addresses
	// the instance (dotted: instance | stack.instance | org.stack.instance |
	// org.stack.env.instance, partial forms resolve from the current
	// context). Presence of from: is what makes an entry a slice; type: is
	// implied ("slice").
	From string `yaml:"from" json:"from,omitempty"`
	// Name is the db/bucket name inside the instance; "" = the config key.
	Name string `yaml:"name" json:"name,omitempty"`
	// OnRemove: keep (default) leaves the data when the entry leaves the
	// file; drop destroys it.
	OnRemove string `yaml:"on_remove" json:"on_remove,omitempty"`
	// Public gives the slice a public-read policy (s3 only).
	Public bool `yaml:"public" json:"public,omitempty"`
}

// SliceName is the slice's db/bucket name with the default applied.
func (tc TileConf) SliceName(key string) string {
	if tc.Name != "" {
		return tc.Name
	}
	return key
}

// removalPolicy normalizes on_remove for comparison: the file says keep|drop,
// rows historically store ""/"detach"/"drop".
func removalPolicy(s string) string {
	if s == "drop" {
		return "drop"
	}
	return "keep"
}

// FromInstance is the instance slug a from: address ends in.
func FromInstance(from string) string {
	if i := strings.LastIndex(from, "."); i >= 0 {
		return from[i+1:]
	}
	return from
}

// EnvMap tolerates scalar YAML values of any type (PORT: 8080 without
// quotes) by stringifying them.
type EnvMap map[string]string

func (m *EnvMap) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("env must be a map")
	}
	*m = EnvMap{}
	for i := 0; i < len(n.Content); i += 2 {
		(*m)[n.Content[i].Value] = n.Content[i+1].Value
	}
	return nil
}

type BuildConf struct {
	Context    string `yaml:"context" json:"context,omitempty"`
	Dockerfile string `yaml:"dockerfile" json:"dockerfile,omitempty"`
}

// DomainConf is one domain claim. Exactly one of Host / Apex / Auto is set:
// a literal host (as always), the bare host of a visible domain resource, or
// a generated name under the nearest visible resource. First listed is the
// primary, what STACKR_PUBLIC_URL resolves to.
type DomainConf struct {
	Host  string `yaml:"host" json:"host,omitempty"`
	Apex  string `yaml:"apex" json:"apex,omitempty"`
	Auto  bool   `yaml:"auto" json:"auto,omitempty"`
	Path  string `yaml:"path" json:"path,omitempty"`
	HTTPS *bool  `yaml:"https" json:"https,omitempty"` // nil = true
	// ForceHTTPS bounces plain HTTP onto the TLS router. Distinct from https:,
	// which is only "serve TLS here". nil = true, which is what serving TLS
	// alone used to do.
	ForceHTTPS *bool  `yaml:"force_https" json:"force_https,omitempty"`
	RedirectTo string `yaml:"redirect_to" json:"redirect_to,omitempty"`
	// Middlewares are appended after stackr's own auth and header ones: a
	// bare name is this stack's proxy.middlewares entry, stack/name another
	// stack's in the same org.
	Middlewares []string `yaml:"middlewares" json:"middlewares,omitempty"`
	Priority    int      `yaml:"priority" json:"priority,omitempty"`
	// Rule is a raw traefik rule that replaces the generated Host/PathPrefix
	// matcher. host: is still required: it picks the certificate.
	Rule string `yaml:"rule" json:"rule,omitempty"`
	// Port is the container port for this entry; 0 inherits the tile's.
	Port int `yaml:"port" json:"port,omitempty"`
}

func (d DomainConf) HTTPSOn() bool { return d.HTTPS == nil || *d.HTTPS }

// ForceHTTPSOn reports whether plain HTTP is bounced; only meaningful when
// HTTPSOn is true.
func (d DomainConf) ForceHTTPSOn() bool { return d.ForceHTTPS == nil || *d.ForceHTTPS }

type LimitsConf struct {
	CPU      float64 `yaml:"cpu" json:"cpu,omitempty"`
	MemoryMB int     `yaml:"memory_mb" json:"memory_mb,omitempty"`
}

// Resolved is the final product: per-environment tile sets, ready to diff.
type Resolved struct {
	Stack string
	// UIEdits is the file's ui_edits:, blank when it says nothing (the org
	// default then applies). See repo.Stack.UIEdits.
	UIEdits string
	// Domains is the stack's declared domain resources, stack-level (not per
	// env), so an env-scoped plan leaves them alone entirely.
	Domains []DomainResConf
	// Middlewares is proxy.middlewares, stack-level like Domains.
	Middlewares map[string]RawMap
	PREnvs      *PREnvs
	EnvOrder    []string // the ladder: first = default environment; the home (repo.HomeSlug) is not on it
	// DefaultEnv is the file's bottom rung, set by Load. Diff prefers it over
	// the store's idea of the default: on a first apply the envs the file
	// declares may not exist yet, and the store's first env can be one the
	// org file created in a different order. Empty for a Resolved built from
	// live state, where the store order is the truth.
	DefaultEnv string
	// Vars is the file's stack-level vars:, plain values only.
	Vars map[string]string
	// Defaults is the file's stack-level defaults:.
	Defaults DefaultsConf
	// Moves are the file's validated moved: entries.
	Moves []Move
	Envs  map[string]ResolvedEnv
	// PRTemplate is the tile set a PR env is built from (base + pr_envs.tiles),
	// nil when pr_envs is absent. Deliberately NOT in Envs, it must never look
	// like a static env to drift detection or the default-env pick.
	PRTemplate *ResolvedEnv
}

type ResolvedEnv struct {
	Protected bool
	Color     string
	// ApplyPolicy is the env's apply_policy:, "" for the per-rung default.
	ApplyPolicy string
	// Defaults is the env's own rung of the defaults cascade.
	Defaults DefaultsConf
	// Declared is what this env's own overlay sets per tile: flattened keys
	// ("image", "env.PORT"). A difference between environments the file
	// declares is on purpose, so the compare panel marks it intended.
	Declared map[string]map[string]bool
	// Secrets is the stack-level declarations plus this env's own (bare) ones.
	Secrets map[string]SecretConf
	// Vars is this env's own vars: only, not merged with the stack's. The
	// resolver does the shadowing, so an env row is written only where the
	// file actually declares one.
	Vars  map[string]string
	Tiles map[string]TileConf
}

// Fetcher loads an included file's raw bytes by repo-relative path.
type Fetcher func(path string) ([]byte, error)

// strictYAML decodes into out rejecting unknown keys. A misspelt top-level key
// (environments with a letter dropped) used to be silently ignored while the
// same typo inside a tile body errored, half the file was checked, half wasn't.
func strictYAML(data []byte, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && err != io.EOF {
		return humanYAML(err)
	}
	return nil
}

// removedKeys names what replaced a key the schema used to carry. A file
// written against an older stackr then says what to do about it rather than
// only that the key is not known.
var removedKeys = map[string]string{
	"compose":        "use image: or build: instead",
	"compose_inline": "use image: or build: instead",
	"compose_path":   "use image: or build: instead",
}

// unknownField matches yaml.v3's wording for a key that is not in the struct.
var unknownField = regexp.MustCompile(`^(?:line \d+: )?field (\S+) not found in type \S+$`)

// humanYAML rewrites yaml.v3's unknown-key errors for someone reading them on
// the config plan page. Its own wording names the Go type the decode targeted
// ("not found in type stackconf.TileConf"), which means nothing to a person
// editing YAML, and its line number counts lines in a re-marshalled fragment
// rather than in the file they wrote. Anything that is not an unknown key is
// passed through untouched: those messages are already about the YAML.
// HumanYAML is humanYAML for the org file, which decodes strictly the same
// way and leaked the same Go type names (orgconf.File, orgconf.StackRef).
func HumanYAML(err error) error { return humanYAML(err) }

func humanYAML(err error) error {
	te, ok := err.(*yaml.TypeError)
	if !ok {
		return err
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		m := unknownField.FindStringSubmatch(e)
		if m == nil {
			msgs = append(msgs, e)
			continue
		}
		s := "unknown key " + m[1]
		if hint := removedKeys[m[1]]; hint != "" {
			s += " (no longer supported, " + hint + ")"
		}
		msgs = append(msgs, s)
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// strictNode is strictYAML for a node reached through a custom unmarshaler,
// which doesn't inherit the parent decoder's KnownFields setting.
func strictNode(n *yaml.Node, out any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return strictYAML(b, out)
}

// Parse decodes and validates one file (includes not yet loaded).
func Parse(data []byte) (*File, error) {
	var f File
	if err := strictYAML(data, &f); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d (want 1)", f.Version)
	}
	if err := ValidateUIEdits(f.UIEdits); err != nil {
		return nil, err
	}
	if err := ValidateDomains(f.Domains); err != nil {
		return nil, err
	}
	if err := validateMiddlewares(f.Proxy.Middlewares); err != nil {
		return nil, err
	}
	return &f, nil
}

// Load parses data, pulls includes via fetch (merged in order, later wins),
// and resolves environment overlays.
func Load(data []byte, fetch Fetcher) (*Resolved, error) {
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for _, inc := range f.Include {
		b, err := fetch(inc)
		if err != nil {
			return nil, fmt.Errorf("include %s: %w", inc, err)
		}
		var extra File
		if err := strictYAML(b, &extra); err != nil {
			return nil, fmt.Errorf("include %s: %w", inc, err)
		}
		if f.Shared == nil {
			f.Shared = map[string]RawMap{}
		}
		for name, raw := range extra.Shared {
			f.Shared[name] = mergeMaps(f.Shared[name], raw)
		}
		if len(extra.Base.Tiles) > 0 && f.Base.Tiles == nil {
			f.Base.Tiles = map[string]RawMap{}
		}
		for name, raw := range extra.Base.Tiles {
			f.Base.Tiles[name] = mergeMaps(f.Base.Tiles[name], raw)
		}
		for name, v := range extra.Vars {
			if f.Vars == nil {
				f.Vars = map[string]string{}
			}
			f.Vars[name] = v
		}
		for _, name := range extra.Environments.Order {
			if _, ok := f.Environments.Envs[name]; !ok {
				f.Environments.Order = append(f.Environments.Order, name)
			}
			if f.Environments.Envs == nil {
				f.Environments.Envs = map[string]EnvConf{}
			}
			f.Environments.Envs[name] = mergeEnvConf(f.Environments.Envs[name], extra.Environments.Envs[name])
		}
	}
	return resolve(f)
}

func resolve(f *File) (*Resolved, error) {
	r := &Resolved{
		Stack:       f.Stack,
		UIEdits:     f.UIEdits,
		Domains:     f.Domains,
		Middlewares: f.Proxy.Middlewares,
		PREnvs:      f.PREnvs,
		Envs:        map[string]ResolvedEnv{},
		Vars:        f.Vars,
		Defaults:    f.Defaults,
	}
	if r.Stack == "" {
		return nil, fmt.Errorf("stack name required")
	}
	order := f.Environments.Order
	if len(order) == 0 {
		order = []string{"production"}
		f.Environments.Envs = map[string]EnvConf{"production": {}}
	}
	r.EnvOrder = order
	r.DefaultEnv = order[0]
	if err := validateVars("stack", f.Vars, f.Secrets); err != nil {
		return nil, err
	}
	moves, err := ParseMoves(f.Moved, "tile", "env")
	if err != nil {
		return nil, err
	}
	r.Moves = moves

	for _, envName := range order {
		if envName == repo.HomeSlug {
			return nil, fmt.Errorf("environment name %q is reserved for the stack's shared tiles", envName)
		}
		ec := f.Environments.Envs[envName]
		if !envcolor.Valid(ec.Color) {
			return nil, fmt.Errorf("env %s: color %q is not a palette name or #rrggbb", envName, ec.Color)
		}
		switch ec.ApplyPolicy {
		case "", "auto", "manual":
		default:
			return nil, fmt.Errorf("env %s: apply_policy %q must be auto or manual", envName, ec.ApplyPolicy)
		}
		re := ResolvedEnv{Protected: ec.Protected, Color: ec.Color, ApplyPolicy: ec.ApplyPolicy,
			Defaults: ec.Defaults, Tiles: map[string]TileConf{},
			Secrets: mergeSecrets(f.Secrets, ec.Secrets), Vars: ec.Vars, Declared: declaredKeys(ec.Tiles)}
		if err := validateVars("env "+envName, ec.Vars, re.Secrets); err != nil {
			return nil, err
		}
		for name, raw := range overlayTiles(f.Base.Tiles, ec.Tiles) {
			tc, err := decodeTile(raw)
			if err != nil {
				return nil, fmt.Errorf("env %s tile %s: %w", envName, name, err)
			}
			if err := validateTile(name, tc); err != nil {
				return nil, fmt.Errorf("env %s: %w", envName, err)
			}
			re.Tiles[name] = tc
		}
		// A name that could never be an environment variable can never be set,
		// so the declaration would warn forever with no way to satisfy it.
		for name, sc := range re.Secrets {
			if !envVarRe.MatchString(name) {
				return nil, fmt.Errorf("env %s: secret %q is not a valid environment variable name", envName, name)
			}
			if sc.Default != "" && sc.Default != "generated" {
				return nil, fmt.Errorf("secret %s: default %q must be empty or \"generated\"", name, sc.Default)
			}
			if sc.Length < 0 {
				return nil, fmt.Errorf("secret %s: length must be positive", name)
			}
			if sc.Default != "generated" && (sc.Length != 0 || sc.IncludeNumbers || sc.IncludeSymbols) {
				return nil, fmt.Errorf("secret %s: length/include_* only apply with default: generated", name)
			}
		}
		r.Envs[envName] = re
	}

	// Shared tiles: stack-scoped managed instances. They live in the stack's
	// home environment, never on a rung of the ladder, so the order of
	// environments: cannot move them. Always present, even empty, so a shared
	// tile dropped from the file still diffs as a delete.
	home := ResolvedEnv{Tiles: map[string]TileConf{}}
	for name, raw := range f.Shared {
		tc, err := decodeTile(raw)
		if err != nil {
			return nil, fmt.Errorf("shared tile %s: %w", name, err)
		}
		if tc.Type != "managed" {
			return nil, fmt.Errorf("shared tile %s: only managed instances can be stack-shared, not type %q", name, tc.Type)
		}
		if err := validateTile(name, tc); err != nil {
			return nil, fmt.Errorf("shared: %w", err)
		}
		tc.Scope = "stack"
		for _, envName := range order {
			if _, dup := r.Envs[envName].Tiles[name]; dup {
				return nil, fmt.Errorf("tile %s declared both under shared: and in env %s", name, envName)
			}
		}
		home.Tiles[name] = tc
	}
	r.Envs[repo.HomeSlug] = home

	// Startup-order graphs are per env, validated after shared tiles merge so
	// a shared instance is a legal dependency target.
	for _, envName := range order {
		if err := validateDeps(envName, r.Envs[envName].Tiles); err != nil {
			return nil, err
		}
	}

	if f.PREnvs != nil {
		tpl := ResolvedEnv{Tiles: map[string]TileConf{}, Secrets: mergeSecrets(f.Secrets, nil)}
		for name, raw := range overlayTiles(f.Base.Tiles, f.PREnvs.Tiles) {
			tc, err := decodeTile(raw)
			if err != nil {
				return nil, fmt.Errorf("pr_envs tile %s: %w", name, err)
			}
			if err := validateTile(name, tc); err != nil {
				return nil, fmt.Errorf("pr_envs: %w", err)
			}
			tpl.Tiles[name] = tc
		}
		if err := validateDeps("pr_envs", tpl.Tiles); err != nil {
			return nil, err
		}
		r.PRTemplate = &tpl
	}
	return r, nil
}

// DecodeTile / ValidateTileConf are the exported halves of tile decoding for
// the org config layer, same strict decode, same rules, no second grammar.
func DecodeTile(raw RawMap) (TileConf, error)         { return decodeTile(raw) }
func ValidateTileConf(name string, tc TileConf) error { return validateTile(name, tc) }

func decodeTile(raw RawMap) (TileConf, error) {
	var tc TileConf
	b, err := yaml.Marshal(map[string]any(raw))
	if err != nil {
		return tc, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&tc); err != nil {
		return tc, humanYAML(err)
	}
	if tc.Type == "" {
		if tc.From != "" {
			tc.Type = "slice"
		} else {
			tc.Type = "service"
		}
	}
	return tc, nil
}

// validateVars rejects names that could never be an environment variable and
// names that are already declared as a secret. The two namespaces are separate
// in a reference (${{ stack.vars.X }} vs ${{ stack.secrets.X }}), so one name in
// both would be a row the file contradicts itself about.
func validateVars(where string, vars map[string]string, secrets map[string]SecretConf) error {
	for _, name := range sortedMapKeys(vars) {
		if !envVarRe.MatchString(name) {
			return fmt.Errorf("%s: var %q is not a valid environment variable name", where, name)
		}
		if _, dup := secrets[name]; dup {
			return fmt.Errorf("%s: %s is declared under both vars: and secrets:; pick one", where, name)
		}
	}
	return nil
}

// sortedMapKeys keeps error messages stable across map iteration order.
func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateBackup checks a tile's backup: block. The destination is validated
// as a reference here (the file is in git, so a literal would be credentials in
// git); whether it resolves to a destination this org may use is a plan
// question, because it needs the store.
func validateBackup(name string, tc TileConf) error {
	b := tc.Backup
	if b == nil {
		return nil
	}
	ref, err := backupDestRef(b.Dest)
	if err != nil {
		return fmt.Errorf("tile %s: backup: %w", name, err)
	}
	_ = ref
	if b.Schedule == "" {
		return fmt.Errorf("tile %s: backup: schedule: is required", name)
	}
	if err := jobs.ValidateCron(b.Schedule); err != nil {
		return fmt.Errorf("tile %s: backup: schedule %q is not a cron expression", name, b.Schedule)
	}
	if b.TZ != "" {
		if _, err := time.LoadLocation(b.TZ); err != nil {
			return fmt.Errorf("tile %s: backup: tz %q is not a timezone", name, b.TZ)
		}
	}
	if b.Keep < 0 {
		return fmt.Errorf("tile %s: backup: keep must not be negative", name)
	}
	// mode: applies while a volume is tarred. A managed database is dumped
	// with its engine's own tool and never pauses, so the key would be a
	// setting that silently does nothing.
	if b.Mode != "" {
		if tc.Type == "managed" {
			return fmt.Errorf("tile %s: backup: mode: applies to a tile with a volume, not to a managed database", name)
		}
		if !backup.ValidMode(b.Mode) {
			return fmt.Errorf("tile %s: backup: mode %q must be pause, stop or live", name, b.Mode)
		}
	}
	return nil
}

// backupDestRef parses a destination reference and returns the name it points
// at. Only the two backup buckets resolve: everything else is either a variable
// (which would put the bucket credentials in the environment) or a literal
// (which would put them in git).
func backupDestRef(dest string) (varref.Ref, error) {
	body := strings.TrimSpace(dest)
	if body == "" {
		return varref.Ref{}, fmt.Errorf("dest: is required, as ${{ org.backups.NAME }} or ${{ stackr.backups.NAME }}")
	}
	refs := varref.Refs(body)
	if len(refs) != 1 || "${{ "+refs[0]+" }}" != body {
		return varref.Ref{}, fmt.Errorf("dest %q must be exactly ${{ org.backups.NAME }} or ${{ stackr.backups.NAME }}", dest)
	}
	r, err := varref.Parse(refs[0])
	if err != nil {
		return varref.Ref{}, err
	}
	if r.Slug != varref.BucketBackups {
		return varref.Ref{}, fmt.Errorf("dest %q must name a backup destination: ${{ org.backups.NAME }} or ${{ stackr.backups.NAME }}", dest)
	}
	return r, nil
}

func validateTile(name string, tc TileConf) error {
	// vars, secrets and backups sit where a source slug goes in a reference,
	// so a tile called one of them would make ${{ stack.vars.X }} ambiguous.
	if varref.Reserved(name) {
		return fmt.Errorf("tile %s: %q is reserved for references (${{ stack.%s.NAME }}); pick another name", name, name, name)
	}
	if err := validateBackup(name, tc); err != nil {
		return err
	}
	switch tc.UpdatePolicy {
	case "", "off", "notify", "auto":
	default:
		return fmt.Errorf("tile %s: update_policy %q must be off, notify or auto", name, tc.UpdatePolicy)
	}
	switch tc.Type {
	case "service", "cron", "function":
		pol := runpolicy.Policies[tc.Type]
		if tc.Build == nil && tc.Image == "" {
			return fmt.Errorf("tile %s: %s needs a build or image source", name, tc.Type)
		}
		if !pol.AllowsIngress && (tc.Port != 0 || len(tc.Domains) > 0) {
			return fmt.Errorf("tile %s: a %s has no endpoint; port: and domains: don't apply", name, tc.Type)
		}
		if !pol.AllowsCommand && tc.Command != "" {
			return fmt.Errorf("tile %s: command: is a %s key", name, "cron")
		}
		if tc.Type != "service" && (tc.User != "" || tc.ShmSizeMB != 0 || tc.Privileged || len(tc.Devices) > 0 || tc.Restart != "") {
			return fmt.Errorf("tile %s: user, shm_size_mb, privileged, devices and restart are service keys", name)
		}
		if _, err := runtime.NormalizeRestart(tc.Restart); err != nil {
			return fmt.Errorf("tile %s: %w", name, err)
		}
		if tc.ShmSizeMB < 0 {
			return fmt.Errorf("tile %s: shm_size_mb must not be negative", name)
		}
		if err := validateReplicas(name, tc); err != nil {
			return err
		}
		for _, d := range tc.Devices {
			if _, err := runtime.ParseDevice(d); err != nil {
				return fmt.Errorf("tile %s: %w", name, err)
			}
		}
		if tc.HealthInterval < 0 || tc.HealthTimeout < 0 || tc.HealthRetries < 0 || tc.HealthStartPeriod < 0 {
			return fmt.Errorf("tile %s: healthcheck knobs must not be negative", name)
		}
		if tc.Type != "function" && tc.RunOnDeploy {
			return fmt.Errorf("tile %s: run_on_deploy is a function key", name)
		}
		if (tc.UpdatePolicy == "notify" || tc.UpdatePolicy == "auto") && tc.Image == "" {
			return fmt.Errorf("tile %s: update_policy watches an image source", name)
		}
		if tc.WaitForCI && tc.Build == nil {
			return fmt.Errorf("tile %s: wait_for_ci needs a git-built source", name)
		}
		for _, d := range tc.DependsOn {
			if _, _, err := ParseDep(d); err != nil {
				return fmt.Errorf("tile %s: %w", name, err)
			}
		}
		if tc.Type != "service" && len(tc.Files) > 0 {
			// crons/functions run as one-shot jobs, which mount nothing.
			return fmt.Errorf("tile %s: files: is a service key", name)
		}
		for _, fl := range tc.Files {
			if _, _, _, err := runtime.ParseFileMount(fl); err != nil {
				return fmt.Errorf("tile %s: %w", name, err)
			}
		}
		if tc.Type != "service" && len(tc.Storage) > 0 {
			return fmt.Errorf("tile %s: storage: is a service key", name)
		}
		for _, sl := range tc.Storage {
			if _, _, _, _, err := storagetiles.ParseAttachment(sl); err != nil {
				return fmt.Errorf("tile %s: %w", name, err)
			}
		}
		if pol.RequiresSchedule {
			if tc.Schedule == "" {
				return fmt.Errorf("tile %s: cron needs a schedule", name)
			}
			// Validated here, not in createTile: updateTile never checked, so an
			// invalid schedule landed in the DB and LoadSchedules silently skipped
			// the job, a cron that looks configured and never runs.
			if err := jobs.ValidateCron(tc.Schedule); err != nil {
				return fmt.Errorf("tile %s: %w", name, err)
			}
			// timeout_minutes is a plain int, so absent and 0 are the same value,
			// both mean the 30-minute default. Negative is a typo.
			if tc.TimeoutMinutes < 0 {
				return fmt.Errorf("tile %s: timeout_minutes must be positive", name)
			}
		} else if tc.Schedule != "" {
			return fmt.Errorf("tile %s: schedule: is a cron key", name)
		}
	case "managed":
		if _, ok := managedtiles.Engines[tc.Engine]; !ok {
			return fmt.Errorf("tile %s: db engine %q is not supported, stackr manages postgres and s3", name, tc.Engine)
		}
		// Caught here rather than at deploy: an out-of-range port reaches
		// docker as a port mapping it rejects, and the failure surfaces as a
		// container that won't start rather than as a bad line in the file.
		if tc.ExternalPort < 0 || tc.ExternalPort > 65535 {
			return fmt.Errorf("tile %s: external_port %d out of range", name, tc.ExternalPort)
		}
		if tc.ShmSizeMB < 0 {
			return fmt.Errorf("tile %s: shm_size_mb must not be negative", name)
		}
		if !validScope(tc.Scope) {
			return fmt.Errorf("tile %s: scope %q must be env, stack or org", name, tc.Scope)
		}
		if tc.WaitForCI {
			return fmt.Errorf("tile %s: wait_for_ci needs a git-built source", name)
		}
	case "volume":
		if tc.Attach != "" && tc.Path == "" {
			return fmt.Errorf("tile %s: attached volume needs a path", name)
		}
	case "slice":
		if tc.From == "" {
			return fmt.Errorf("tile %s: slice needs a from: instance address", name)
		}
		if n := strings.Count(tc.From, ".") + 1; n > 4 {
			return fmt.Errorf("tile %s: from %q is not an instance address (instance, stack.instance, org.stack.instance or org.stack.env.instance)", name, tc.From)
		}
		for _, seg := range strings.Split(tc.From, ".") {
			if seg == "" {
				return fmt.Errorf("tile %s: from %q has an empty segment", name, tc.From)
			}
		}
		if tc.OnRemove != "" && tc.OnRemove != "keep" && tc.OnRemove != "drop" {
			return fmt.Errorf("tile %s: on_remove %q must be keep or drop", name, tc.OnRemove)
		}
	default:
		return fmt.Errorf("tile %s: unknown type %q", name, tc.Type)
	}
	for _, d := range tc.Domains {
		n := 0
		if d.Host != "" {
			n++
		}
		if d.Apex != "" {
			n++
		}
		if d.Auto {
			n++
		}
		if n != 1 {
			return fmt.Errorf("tile %s: a domain entry sets exactly one of host, apex or auto", name)
		}
		if (d.Apex != "" || d.Auto) && (d.Path != "" || d.RedirectTo != "") {
			return fmt.Errorf("tile %s: apex/auto domains take no path or redirect", name)
		}
		if d.Rule != "" && d.Host == "" {
			return fmt.Errorf("tile %s: a domain rule needs host: too, it picks the certificate", name)
		}
		for _, m := range d.Middlewares {
			if strings.Count(m, "/") > 1 || strings.HasPrefix(m, "/") || strings.HasSuffix(m, "/") || strings.TrimSpace(m) == "" {
				return fmt.Errorf("tile %s: middleware %q must be name or stack/name", name, m)
			}
		}
	}
	return nil
}

// envVarRe is a shell-style identifier, what an injected variable name has to
// be for the container to see it at all.
var envVarRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// mergeSecrets unions the stack-level declarations with an environment's own
// bare ones. Options live at stack level only, an env adds names, never
// redefines behavior.
func mergeSecrets(stack SecretsNode, env []string) map[string]SecretConf {
	if len(stack) == 0 && len(env) == 0 {
		return nil
	}
	out := make(map[string]SecretConf, len(stack)+len(env))
	for name, sc := range stack {
		out[name] = sc
	}
	for _, s := range env {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, declared := out[s]; !declared {
			out[s] = SecretConf{}
		}
	}
	return out
}

// validScope reports whether s is a sharing scope for a db instance. "" is the
// unset form and means env, matching the tile row's default.
func validScope(s string) bool {
	return s == "" || s == "env" || s == "stack" || s == "org"
}

// mergeMaps deep-merges overlay onto base (maps merge recursively, anything
// else (including lists) replaces wholesale).
func mergeMaps(base, overlay RawMap) RawMap {
	if base == nil {
		return overlay
	}
	out := RawMap{}
	for k, v := range base {
		out[k] = v
	}
	for k, ov := range overlay {
		if bm, ok := asMap(out[k]); ok {
			if om, ok2 := asMap(ov); ok2 {
				out[k] = mergeMaps(bm, om)
				continue
			}
		}
		out[k] = ov
	}
	return out
}

// asMap normalizes the two map types yaml decoding produces (nested maps
// inherit the parent's named type).
func asMap(v any) (RawMap, bool) {
	switch m := v.(type) {
	case RawMap:
		return m, true
	case map[string]any:
		return m, true
	}
	return nil, false
}

// overlayTiles merges an env's (or the PR template's) tile entries onto the
// base tiles: exclusion drops the base tile, a body deep-merges onto it,
// names only in the overlay pass through as-is.
// declaredKeys flattens an env overlay to the keys it sets per tile.
func declaredKeys(overlay map[string]TileNode) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for name, node := range overlay {
		if node.Excluded || len(node.Raw) == 0 {
			continue
		}
		keys := map[string]bool{}
		for k, v := range node.Raw {
			if k == "env" || k == "limits" {
				if sub := subKeys(v); len(sub) > 0 {
					for _, sk := range sub {
						keys[k+"."+sk] = true
					}
					continue
				}
			}
			keys[k] = true
		}
		out[name] = keys
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// subKeys is the string keys of a decoded yaml map, whichever map type the
// decoder produced for it.
func subKeys(v any) []string {
	var out []string
	switch m := v.(type) {
	case map[string]any:
		for k := range m {
			out = append(out, k)
		}
	case RawMap:
		for k := range m {
			out = append(out, k)
		}
	case map[any]any:
		for k := range m {
			out = append(out, fmt.Sprint(k))
		}
	}
	return out
}

func overlayTiles(base map[string]RawMap, overlay map[string]TileNode) map[string]RawMap {
	out := make(map[string]RawMap, len(base)+len(overlay))
	for name, raw := range base {
		out[name] = raw
	}
	for name, node := range overlay {
		if node.Excluded {
			delete(out, name)
			continue
		}
		out[name] = mergeMaps(out[name], node.Raw)
	}
	return out
}

func mergeEnvConf(base, overlay EnvConf) EnvConf {
	if overlay.Protected {
		base.Protected = true
	}
	if overlay.ApplyPolicy != "" {
		base.ApplyPolicy = overlay.ApplyPolicy
	}
	for k, v := range overlay.Vars {
		if base.Vars == nil {
			base.Vars = map[string]string{}
		}
		base.Vars[k] = v
	}
	if base.Tiles == nil {
		base.Tiles = overlay.Tiles
	} else {
		for k, v := range overlay.Tiles {
			base.Tiles[k] = v
		}
	}
	return base
}

// EnvLines renders a tile's env map as sorted KEY=VALUE lines, the storage
// format used by repo.Tile.Env.
func EnvLines(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(env[k])
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// validateReplicas refuses replicas above 1 on a tile the pinned classifier
// would call pinned.
//
// Refused at parse rather than silently forced back to 1
// (docs/plans/31-node-agent-open-questions.md, replicas against the pinned
// classifier). A volume is a directory on one host's disk and one container
// at a time may write it; a config that asks for three and gets one has been
// lied to about what is running.
//
// The classifier here is the config's own view of the tile, because the tile
// row may not exist yet: a volume: line, or a volume tile pointing at it. The
// full classifier (infra/placement) sees the same two facts once it does.
func validateReplicas(name string, tc TileConf) error {
	if tc.Replicas < 0 {
		return fmt.Errorf("tile %s: replicas must not be negative", name)
	}
	if tc.Replicas <= 1 {
		return nil
	}
	if tc.Type != "service" {
		return fmt.Errorf("tile %s: replicas is a service key", name)
	}
	if len(tc.Volumes) > 0 {
		return fmt.Errorf("tile %s: replicas: %d, but it mounts a volume; a volume is on one machine's disk "+
			"and one container at a time may write it. Drop the volume or set replicas: 1", name, tc.Replicas)
	}
	return nil
}
