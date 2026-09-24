# config/stackconf

Source: `config/stackconf/` — `stackconf.go`, `plan.go`, `deps.go`, `backups.go`, `slices.go` (3283 of the package's 10991 lines).
Commit: c2423f0.
Taken: the file grammar (every key, type, validation), include + per-env overlay merge, the plan diff (tile create/update/delete, domains, slices), `depends_on` validation and topological order, the backup diff rules, the slice rules.
Cut: `apply.go` (job wiring, Swarm calls), `staging.go`, `moved.go`, `job.go`, `runner.go`, `export.go`, `serialize.go`, `middlewares.go`, `apply_policy`, `traefik_override`, `ui_edits`, and every store/Docker call inside `backups.go` and `slices.go`.
Cuts belong to: the promote flow's reconcile step (the applier), `service/internal/store` (every store read), `infra/docker` (every container call), the job/connector layer (`job.go`, `runner.go`).

Comments below are kept only where they carry a decision. Every key is listed
once in "File grammar as today"; every diff rule once in "Plan-diff rules".

---

## Kept code

### Grammar: top level (`stackconf.go`)

```go
// Merge model: tiles stay raw YAML maps until after include- and env-overlay
// merging, then decode into typed TileConf. That is what makes sparse overlays
// trivial — an overlay only overrides the keys it mentions.
package stackconf

const DefaultPath = "stackr-compose.yml"

type File struct {
	Version int      `yaml:"version"` // must be 1
	Stack   string   `yaml:"stack"`
	PREnvs  *PREnvs  `yaml:"pr_envs,omitempty"`
	Include []string `yaml:"include,omitempty"`
	// extract: grammar changes per plan: secrets: and vars: collapse into one
	// `params: <collection>: <name>: {type: param|secret, value}` block. Keep
	// the rule the split carried: a secret declares name + type only, never a
	// value (the file is in git); a plain param may carry its value.
	Secrets  SecretsNode       `yaml:"secrets,omitempty"`
	Vars     map[string]string `yaml:"vars,omitempty"`
	Defaults DefaultsConf      `yaml:"defaults,omitempty"`
	// extract: dropped moved:, belongs nowhere — orphaning covers the
	// data-loss case and re-adding a slug re-adopts.
	Moved        []MovedEntry `yaml:"moved,omitempty"`
	Environments EnvsNode     `yaml:"environments"`
	// Shared is stack-scoped: managed instances every env shares. Scope is
	// structural — an entry here is stack-scoped, a tile under an environment
	// is env-scoped. The row lives in the stack's home env.
	Shared map[string]RawMap `yaml:"shared,omitempty"`
	// Base lands in EVERY static env; an env entry of the same name is a
	// sparse overlay or an exclusion (false). Unlike shared:, each env gets
	// its own copy.
	Base    BaseConf        `yaml:"base,omitempty"`
	Domains []DomainResConf `yaml:"domains,omitempty"`
	// extract: dropped ui_edits, belongs nowhere — drift never promotes.
	UIEdits string `yaml:"ui_edits,omitempty"`
	// extract: dropped proxy.middlewares (raw traefik bodies), belongs in the
	// admin-only proxy config outside the file.
	Proxy ProxyConf `yaml:"proxy,omitempty"`
}

// DomainResConf: hosts are unique across the whole server, so a clash is a
// plan error, not an apply error.
type DomainResConf struct {
	Host                string `yaml:"host"`
	ACMEEmail           string `yaml:"acme_email,omitempty"`
	IncludeEnvOnDefault bool   `yaml:"include_env_on_default"` // api.prod.stack.org vs api.stack.org
}

func ValidateDomains(ds []DomainResConf) error {
	seen := map[string]bool{}
	for _, d := range ds {
		if strings.TrimSpace(d.Host) == "" { return fmt.Errorf("domains: host required") }
		if seen[d.Host] { return fmt.Errorf("domains: %s declared twice", d.Host) }
		seen[d.Host] = true
	}
	return nil
}

type BaseConf struct{ Tiles map[string]RawMap `yaml:"tiles"` }

// PREnvs: a PR env is base + Tiles, built purely from the file, never cloned
// from a live env, so panel drift cannot leak into previews.
type PREnvs struct {
	// nil = inherit the panel setting. A plain bool made `pr_envs: {tiles: …}`
	// silently disable PR envs, since the file wins over the panel.
	Enabled *bool               `yaml:"enabled"`
	Comment *bool               `yaml:"comment"`
	Status  *bool               `yaml:"status"`
	Against []string            `yaml:"against"` // PR target branches; empty = any
	Tiles   map[string]TileNode `yaml:"tiles"`
}

// RawMap is a tile body kept unmerged.
type RawMap map[string]any

// EnvsNode accepts both forms, order preserved, first env is the default:
//	environments: [production, staging]
//	environments: {production: {…}, staging: {…}}
type EnvsNode struct {
	Order []string
	Envs  map[string]EnvConf
}

type EnvConf struct {
	Protected bool         `yaml:"protected,omitempty"`
	Color     string       `yaml:"color,omitempty"`
	Secrets   []string     `yaml:"secrets,omitempty"`
	Defaults  DefaultsConf `yaml:"defaults,omitempty"`
	// extract: dropped apply_policy (auto|manual), replaced by per-env `from:`
	// (a branch, or `promote`) and `auto:` (bool).
	ApplyPolicy string `yaml:"apply_policy,omitempty"`
	// Vars shadow the stack-level vars: at resolution time. Needs the map form
	// of environments:.
	Vars  map[string]string   `yaml:"vars,omitempty"`
	Tiles map[string]TileNode `yaml:"tiles,omitempty"`
}

// DefaultsConf is one rung of the cascade. Every field is a pointer: nil =
// "say nothing here", which is what lets a level inherit rather than reset the
// level above to a zero value. The same struct at org, stack and env level, so
// a knob cannot mean one thing in the org file and another in the stack file.
type DefaultsConf struct {
	CronTimeoutMin       *int     `yaml:"cron_timeout_min"`
	CPULimit             *float64 `yaml:"cpu_limit"`
	MemLimitMB           *int     `yaml:"mem_limit_mb"`
	RunRetentionDays     *int     `yaml:"run_retention_days"`
	MetricRetentionHours *int     `yaml:"metric_retention_hours"`
	Protect              *bool    `yaml:"protect"`
	ProtectUser          *string  `yaml:"protect_user"`
	ProtectPassword      *string  `yaml:"protect_password"`
	NodeGroup            *string  `yaml:"node_group"`
}

// SettingsJSON renders the level for storage (json.Marshal, "{}" on error).
// build_node is deliberately not a config key: instance-wide, server level only.
func (d DefaultsConf) SettingsJSON() string { … }

// Empty: the file said nothing at this level, which is NOT "inherit
// everything" — a level that declares nothing leaves whatever the panel set.
func (d DefaultsConf) Empty() bool { return d.SettingsJSON() == "{}" }

// Check refuses a level that sets half the basic-auth pair. This is the config
// path's only gate: a user with no password locks every URL below the level
// behind a password nobody has.
func (d DefaultsConf) Check() error { return settings.Parse(d.SettingsJSON()).Check() }
```

### Grammar: params, backups (`stackconf.go`)

```go
// BackupConf. The destination is a reference and never a literal: a
// destination carries bucket credentials and the file is in git.
//
// Kind is not declared. A managed database is dumped with its engine's own
// tool; anything else with a volume gets a tar of that volume. There is no
// third answer, and asking the file to repeat it invites the two disagreeing.
//
// extract: grammar changes per plan: backup: hangs under a `volumes:` entry,
// not under a tile. Everything below is unchanged except that "the tile that
// owns it" becomes "the volume that owns it"; the managed-database dump keeps
// its home on the managed tile.
type BackupConf struct {
	Dest     string `yaml:"dest"`     // ${{ org.backups.NAME }} or ${{ stackr.backups.NAME }}
	Schedule string `yaml:"schedule"` // cron
	TZ       string `yaml:"tz,omitempty"`
	Keep     int    `yaml:"keep,omitempty"` // archives surviving a prune; 0 = all
	Mode     string `yaml:"mode,omitempty"` // pause (default) | stop | live, while the volume is tarred
	// Enabled defaults true; `enabled: false` keeps the schedule declared and
	// stops it firing (dropping the block is how you turn it off for good).
	Enabled *bool `yaml:"enabled,omitempty"`
}

func (b BackupConf) On() bool { return b.Enabled == nil || *b.Enabled }

// SecretConf. Zero value = bare declaration: set out of band, warn while unset.
// extract: grammar changes per plan: one entry of a params collection with
// `type: secret`; these stay its knobs.
type SecretConf struct {
	// "" = set out of band; "generated" = minted on the first apply that finds
	// it unset, then stable forever. Later applies never touch it, and
	// changing the generation knobs never rotates an existing value.
	Default        string `yaml:"default,omitempty"`
	Length         int    `yaml:"length,omitempty"` // generated only; 0 = 32
	IncludeNumbers bool   `yaml:"include_numbers,omitempty"`
	IncludeSymbols bool   `yaml:"include_symbols,omitempty"`
	// Required marks the plan row "Required." and nothing more: the apply goes
	// ahead, and the tiles that read the value fail their own deploy on an
	// unresolved reference. Refusing would mean a stack declaring its own
	// credentials could never be stood up from its file in the first place.
	Required bool `yaml:"required,omitempty"`
	// EnvVersions (default true): each env holds its own value; generation
	// mints one per env. false = one stack-wide value, env overrides refused.
	EnvVersions *bool `yaml:"env_versions,omitempty"`
}

func (sc SecretConf) PerEnv() bool   { return sc.EnvVersions == nil || *sc.EnvVersions }
func (sc SecretConf) GenLength() int { if sc.Length > 0 { return sc.Length }; return 32 }

// SecretsNode: a key with a null body is a bare declaration. Custom
// unmarshaler, because a nested node does not inherit KnownFields.
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

// TileNode is an env entry: an exclusion (false/null) or a tile body.
type TileNode struct {
	Excluded bool
	Raw      RawMap
}

func (t *TileNode) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var b bool
		if err := n.Decode(&b); err == nil && !b { t.Excluded = true; return nil }
		if n.Tag == "!!null" { t.Excluded = true; return nil }
		return fmt.Errorf("tile entry must be a map or false, got %q", n.Value)
	case yaml.MappingNode:
		return n.Decode(&t.Raw)
	}
	return fmt.Errorf("tile entry must be a map or false")
}
```

### Grammar: the tile (`stackconf.go`)

```go
// TileConf is one fully-merged tile definition.
type TileConf struct {
	Type   string     `yaml:"type"` // service (default) | cron | function | managed | volume; from: implies slice
	Build  *BuildConf `yaml:"build"`
	Branch string     `yaml:"branch"`
	Image  string     `yaml:"image"`
	// Config-managed stacks leave these empty and inherit the bound
	// repo/connector (via DiffOpts); UI-managed tiles carry them explicitly so
	// a UI git service round-trips through create + diff.
	GitURL          string       `yaml:"git_url"`
	Connector       string       `yaml:"connector"`
	Port            int          `yaml:"port"`
	Domains         []DomainConf `yaml:"domains"`
	Env             EnvMap       `yaml:"env"`
	Limits          *LimitsConf  `yaml:"limits"`
	Healthcheck     string       `yaml:"healthcheck"`
	SecurityHeaders bool         `yaml:"security_headers"`
	Volumes         []string     `yaml:"volumes"`
	WatchPaths      []string     `yaml:"watch_paths"` // regex per line, "!" prefix = ignore
	// nil means the file says nothing; on a config-owned tile that is a
	// delete, same as any other key it stops declaring.
	// extract: grammar changes per plan: moves under the volumes: block.
	Backup *BackupConf `yaml:"backup"`
	// extract: grammar changes per plan: the set becomes manual | auto — no
	// "notify", no "off"; the chip shows either way. Mode 2 adds
	// `tag_policy: semver ^1.2` next to it.
	UpdatePolicy string `yaml:"update_policy"`
	WaitForCI    bool   `yaml:"wait_for_ci"`
	BuildArgs      string `yaml:"build_args"`
	PublishedPorts string `yaml:"published_ports"`
	// extract: dropped traefik_override, belongs in the admin-only proxy
	// config outside the file.
	TraefikOverride string `yaml:"traefik_override"`
	// extract: grammar changes per plan: basic_auth_* and security_headers
	// fold into the named tile `proxy:` extras block (basic_auth, websockets,
	// max_body, timeouts, headers, methods, strip_prefix). Same values, one
	// named home.
	BasicAuthUser     string `yaml:"basic_auth_user"`
	BasicAuthPassword string `yaml:"basic_auth_password"` // plain or a ${{ }} ref, hashed when the route is written
	User       string   `yaml:"user"`
	ShmSizeMB  int      `yaml:"shm_size_mb"`
	Privileged bool     `yaml:"privileged"`
	Devices    []string `yaml:"devices"` // "host[:container[:perms]]"
	Restart    string   `yaml:"restart"` // "" | always | on-failure | no
	// Replicas above 1 on a tile holding a volume is a config error, not
	// something quietly forced back to 1: one mounter per volume, always.
	Replicas  int    `yaml:"replicas"`
	NodeGroup string `yaml:"node_group"`
	HealthInterval    int  `yaml:"healthcheck_interval"` // seconds, 0 = default
	HealthTimeout     int  `yaml:"healthcheck_timeout"`
	HealthRetries     int  `yaml:"healthcheck_retries"`
	HealthStartPeriod int  `yaml:"healthcheck_start_period"`
	RunOnDeploy       bool `yaml:"run_on_deploy"` // function: run again when its own deploy finishes
	DependsOn []string `yaml:"depends_on"` // "slug" or "slug:started|healthy|completed"
	// Files ships repo files into the container, fetched from the tile's git
	// coordinates at deploy, bind-mounted read-only; ":template" runs the
	// varref resolver over the file bytes.
	Files   FileList `yaml:"files"`
	Storage []string `yaml:"storage"` // "storage-slug/path-name:/mount[:ro]"
	Schedule       string `yaml:"schedule"` // cron
	Command        string `yaml:"command"`  // a cron's one-shot line or a service's CMD override
	TimeoutMinutes int    `yaml:"timeout_minutes"`
	AllowOverlap   bool   `yaml:"allow_overlap"`
	Engine       string `yaml:"engine"`        // managed
	ExternalPort int    `yaml:"external_port"` // publish on the host; 0 = internal only
	// Scope is structural in the file (shared: = stack, under an env = env),
	// so it is not a YAML key; resolve() sets it.
	Scope string `yaml:"-"`
	Attach     string `yaml:"attach"`      // volume: tile slug mounting it ("" = detached)
	Path       string `yaml:"path"`        // volume: container mount path
	VolumeName string `yaml:"volume_name"` // pins the docker volume name; empty derives from tile id
	MaxSizeMB  int    `yaml:"max_size_mb"`
	// slice: the config key is the reference slug (${{ tile.<key>.<OUTPUT> }});
	// From addresses the instance (instance | stack.instance |
	// org.stack.instance | org.stack.env.instance, partial forms resolve from
	// context). Presence of from: is what makes an entry a slice.
	//
	// extract: grammar changes per plan: `from:` is now also an ENV-level key
	// (a branch, or `promote`). Different level, no collision — but the two
	// must not be validated by one rule.
	From     string `yaml:"from"`
	Name     string `yaml:"name"`      // db/bucket name inside the instance; "" = the config key
	OnRemove string `yaml:"on_remove"` // keep (default) | drop
	Public   bool   `yaml:"public"`    // public-read policy (s3 only)
}

func (tc TileConf) SliceName(key string) string { if tc.Name != "" { return tc.Name }; return key }

// removalPolicy normalizes on_remove for comparison: the file says keep|drop,
// rows historically store ""/"detach"/"drop".
func removalPolicy(s string) string { if s == "drop" { return "drop" }; return "keep" }

// FromInstance is the instance slug a from: address ends in.
func FromInstance(from string) string {
	if i := strings.LastIndex(from, "."); i >= 0 { return from[i+1:] }
	return from
}

// EnvMap tolerates scalar YAML values of any type (PORT: 8080 unquoted) by
// stringifying them.
type EnvMap map[string]string

func (m *EnvMap) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode { return fmt.Errorf("env must be a map") }
	*m = EnvMap{}
	for i := 0; i < len(n.Content); i += 2 { (*m)[n.Content[i].Value] = n.Content[i+1].Value }
	return nil
}

type BuildConf struct {
	Context    string `yaml:"context"`
	Dockerfile string `yaml:"dockerfile"`
}

// DomainConf is one domain claim. Exactly one of Host / Apex / Auto: a literal
// host, the bare host of a visible domain resource, or a generated name under
// the nearest visible resource. First listed is the primary — what
// STACKR_PUBLIC_URL resolves to.
type DomainConf struct {
	Host  string `yaml:"host"`
	Apex  string `yaml:"apex"`
	Auto  bool   `yaml:"auto"`
	Path  string `yaml:"path"`
	HTTPS *bool  `yaml:"https"` // nil = true
	// ForceHTTPS bounces plain HTTP onto the TLS router. Distinct from https:,
	// which is only "serve TLS here". nil = true.
	ForceHTTPS  *bool    `yaml:"force_https"`
	RedirectTo  string   `yaml:"redirect_to"`
	Priority    int      `yaml:"priority"`
	Port        int      `yaml:"port"` // container port for this entry; 0 inherits the tile's
	// extract: dropped rule: and middlewares:, raw-proxy keys — belongs in the
	// admin-only proxy config.
	Middlewares []string `yaml:"middlewares"`
	Rule        string   `yaml:"rule"`
}

func (d DomainConf) HTTPSOn() bool      { return d.HTTPS == nil || *d.HTTPS }
func (d DomainConf) ForceHTTPSOn() bool { return d.ForceHTTPS == nil || *d.ForceHTTPS }

type LimitsConf struct {
	CPU      float64 `yaml:"cpu"`
	MemoryMB int     `yaml:"memory_mb"`
}

// FileList: YAML takes the map form (repo/path: /container/path[:template]) or
// a list of "repo/path:/container/path[:template]" lines; internally always
// the line form, sorted for the map case so rows and diffs stay canonical.
type FileList []string
```

### Resolved output (`stackconf.go`)

```go
// Resolved is the final product: per-environment tile sets, ready to diff.
type Resolved struct {
	Stack    string
	Domains  []DomainResConf // stack-level, so an env-scoped plan leaves them alone
	PREnvs   *PREnvs
	EnvOrder []string // the ladder: first = default env; the home slug is not on it
	// DefaultEnv is the FILE's bottom rung. Diff prefers it over the store's
	// idea of the default: on a first apply the envs the file declares may not
	// exist yet, and the store's first env can be one the org file created in
	// a different order. Empty for a Resolved built from live state.
	DefaultEnv string
	Vars       map[string]string
	Defaults   DefaultsConf
	Envs       map[string]ResolvedEnv
	// PRTemplate is the tile set a PR env is built from (base + pr_envs.tiles),
	// nil when pr_envs is absent. Deliberately NOT in Envs: it must never look
	// like a static env to drift detection or the default-env pick.
	PRTemplate *ResolvedEnv
	// extract: dropped UIEdits, Middlewares, Moves — cut keys.
}

type ResolvedEnv struct {
	Protected bool
	Color     string
	Defaults  DefaultsConf
	// Declared is what this env's own overlay sets per tile, flattened
	// ("image", "env.PORT"). A difference between envs the file declares is on
	// purpose, so the compare view marks it intended.
	Declared map[string]map[string]bool
	Secrets  map[string]SecretConf // stack-level declarations plus this env's bare ones
	// Vars is this env's own vars: only, not merged with the stack's. The
	// resolver does the shadowing, so an env row is written only where the
	// file actually declares one.
	Vars  map[string]string
	Tiles map[string]TileConf
	// extract: dropped ApplyPolicy, replaced by from:/auto:.
}

// Fetcher loads an included file's raw bytes by repo-relative path.
type Fetcher func(path string) ([]byte, error)
```

### Strict decode and human errors (`stackconf.go`)

```go
// strictYAML decodes rejecting unknown keys. A misspelt TOP-LEVEL key used to
// be silently ignored while the same typo inside a tile body errored: half the
// file was checked, half was not.
func strictYAML(data []byte, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && err != io.EOF { return humanYAML(err) }
	return nil
}

// strictNode is strictYAML for a node reached through a custom unmarshaler,
// which does not inherit the parent decoder's KnownFields setting.
func strictNode(n *yaml.Node, out any) error { /* yaml.Marshal(n) → strictYAML */ }

// removedKeys names what replaced a key the schema used to carry, so an old
// file is told what to do, not only that the key is unknown.
var removedKeys = map[string]string{
	"compose":        "use image: or build: instead",
	"compose_inline": "use image: or build: instead",
	"compose_path":   "use image: or build: instead",
}

var unknownField = regexp.MustCompile(`^(?:line \d+: )?field (\S+) not found in type \S+$`)

// humanYAML rewrites yaml.v3's unknown-key errors for someone reading them on
// the plan page: its wording names the Go type the decode targeted ("not found
// in type stackconf.TileConf"), which means nothing to a person editing YAML,
// and its line number counts lines in a re-marshalled fragment, not in the
// file they wrote. Anything that is not an unknown key passes through: those
// messages are already about the YAML.
func humanYAML(err error) error {
	te, ok := err.(*yaml.TypeError)
	if !ok { return err }
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		m := unknownField.FindStringSubmatch(e)
		if m == nil { msgs = append(msgs, e); continue }
		s := "unknown key " + m[1]
		if hint := removedKeys[m[1]]; hint != "" { s += " (no longer supported, " + hint + ")" }
		msgs = append(msgs, s)
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}
```

### Parse, include merge, env overlay (`stackconf.go`)

```go
func Parse(data []byte) (*File, error) {
	var f File
	if err := strictYAML(data, &f); err != nil { return nil, fmt.Errorf("yaml: %w", err) }
	if f.Version != 1 { return nil, fmt.Errorf("unsupported version %d (want 1)", f.Version) }
	if err := ValidateDomains(f.Domains); err != nil { return nil, err }
	if err := f.Defaults.Check(); err != nil { return nil, fmt.Errorf("defaults: %w", err) }
	for _, name := range f.Environments.Order {
		if err := f.Environments.Envs[name].Defaults.Check(); err != nil {
			return nil, fmt.Errorf("environment %s defaults: %w", name, err)
		}
	}
	return &f, nil
	// extract: dropped ValidateUIEdits and validateMiddlewares calls, cut keys.
}

// Load parses data, pulls includes via fetch (merged in order, later wins),
// and resolves environment overlays.
func Load(data []byte, fetch Fetcher) (*Resolved, error) {
	f, err := Parse(data)
	if err != nil { return nil, err }
	for _, inc := range f.Include {
		b, err := fetch(inc)
		if err != nil { return nil, fmt.Errorf("include %s: %w", inc, err) }
		var extra File
		if err := strictYAML(b, &extra); err != nil { return nil, fmt.Errorf("include %s: %w", inc, err) }
		if f.Shared == nil { f.Shared = map[string]RawMap{} }
		for name, raw := range extra.Shared { f.Shared[name] = mergeMaps(f.Shared[name], raw) }
		if len(extra.Base.Tiles) > 0 && f.Base.Tiles == nil { f.Base.Tiles = map[string]RawMap{} }
		for name, raw := range extra.Base.Tiles { f.Base.Tiles[name] = mergeMaps(f.Base.Tiles[name], raw) }
		for name, v := range extra.Vars {
			if f.Vars == nil { f.Vars = map[string]string{} }
			f.Vars[name] = v
		}
		for _, name := range extra.Environments.Order {
			if _, ok := f.Environments.Envs[name]; !ok {
				f.Environments.Order = append(f.Environments.Order, name)
			}
			if f.Environments.Envs == nil { f.Environments.Envs = map[string]EnvConf{} }
			f.Environments.Envs[name] = mergeEnvConf(f.Environments.Envs[name], extra.Environments.Envs[name])
		}
	}
	return resolve(f)
	// NOTE: an include contributes shared:, base.tiles:, vars: and
	// environments: ONLY. Its secrets:, defaults:, domains: and pr_envs: are
	// silently dropped today — keep or close deliberately.
}

func resolve(f *File) (*Resolved, error) {
	r := &Resolved{Stack: f.Stack, Domains: f.Domains, PREnvs: f.PREnvs,
		Envs: map[string]ResolvedEnv{}, Vars: f.Vars, Defaults: f.Defaults}
	if r.Stack == "" { return nil, fmt.Errorf("stack name required") }
	order := f.Environments.Order
	if len(order) == 0 { // a file with no environments: gets one
		order = []string{"production"}
		f.Environments.Envs = map[string]EnvConf{"production": {}}
	}
	r.EnvOrder, r.DefaultEnv = order, order[0]
	if err := validateVars("stack", f.Vars, f.Secrets); err != nil { return nil, err }

	for _, envName := range order {
		if envName == repo.HomeSlug {
			return nil, fmt.Errorf("environment name %q is reserved for the stack's shared tiles", envName)
		}
		ec := f.Environments.Envs[envName]
		if !envcolor.Valid(ec.Color) {
			return nil, fmt.Errorf("env %s: color %q is not a palette name or #rrggbb", envName, ec.Color)
		}
		re := ResolvedEnv{Protected: ec.Protected, Color: ec.Color, Defaults: ec.Defaults,
			Tiles: map[string]TileConf{}, Secrets: mergeSecrets(f.Secrets, ec.Secrets),
			Vars: ec.Vars, Declared: declaredKeys(ec.Tiles)}
		if err := validateVars("env "+envName, ec.Vars, re.Secrets); err != nil { return nil, err }
		for name, raw := range overlayTiles(f.Base.Tiles, ec.Tiles) {
			tc, err := decodeTile(raw)
			if err != nil { return nil, fmt.Errorf("env %s tile %s: %w", envName, name, err) }
			if err := validateTile(name, tc); err != nil { return nil, fmt.Errorf("env %s: %w", envName, err) }
			re.Tiles[name] = tc
		}
		// Per secret: the name must be a shell identifier — one that could
		// never be an environment variable can never be set, so the
		// declaration would warn forever with no way to satisfy it — default
		// must be "" or "generated", length must be positive, and the
		// generation knobs are refused unless default: generated.
		…
		r.Envs[envName] = re
	}

	// Shared tiles live in the stack's home environment, never on a rung, so
	// the order of environments: cannot move them. Always present, even empty,
	// so a shared tile dropped from the file still diffs as a delete.
	home := ResolvedEnv{Tiles: map[string]TileConf{}}
	for name, raw := range f.Shared {
		tc, err := decodeTile(raw)
		if err != nil { return nil, fmt.Errorf("shared tile %s: %w", name, err) }
		if tc.Type != "managed" {
			return nil, fmt.Errorf("shared tile %s: only managed instances can be stack-shared, not type %q", name, tc.Type)
		}
		if err := validateTile(name, tc); err != nil { return nil, fmt.Errorf("shared: %w", err) }
		tc.Scope = "stack"
		for _, envName := range order {
			if _, dup := r.Envs[envName].Tiles[name]; dup {
				return nil, fmt.Errorf("tile %s declared both under shared: and in env %s", name, envName)
			}
		}
		home.Tiles[name] = tc
	}
	r.Envs[repo.HomeSlug] = home

	// Startup-order graphs are per env, validated AFTER shared tiles merge so
	// a shared instance is a legal dependency target.
	for _, envName := range order {
		if err := validateDeps(envName, r.Envs[envName].Tiles); err != nil { return nil, err }
	}

	if f.PREnvs != nil {
		tpl := ResolvedEnv{Tiles: map[string]TileConf{}, Secrets: mergeSecrets(f.Secrets, nil)}
		for name, raw := range overlayTiles(f.Base.Tiles, f.PREnvs.Tiles) {
			tc, err := decodeTile(raw)
			if err != nil { return nil, fmt.Errorf("pr_envs tile %s: %w", name, err) }
			if err := validateTile(name, tc); err != nil { return nil, fmt.Errorf("pr_envs: %w", err) }
			tpl.Tiles[name] = tc
		}
		if err := validateDeps("pr_envs", tpl.Tiles); err != nil { return nil, err }
		r.PRTemplate = &tpl
	}
	return r, nil
	// extract: dropped ParseMoves/r.Moves (moved: is cut) and ApplyPolicy
	// validation (replaced by from:/auto:).
}

func decodeTile(raw RawMap) (TileConf, error) {
	var tc TileConf
	b, err := yaml.Marshal(map[string]any(raw))
	if err != nil { return tc, err }
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&tc); err != nil { return tc, humanYAML(err) }
	if tc.Type == "" {
		if tc.From != "" { tc.Type = "slice" } else { tc.Type = "service" }
	}
	return tc, nil
}

// mergeMaps deep-merges overlay onto base: maps merge recursively, anything
// else (lists included) replaces wholesale.
func mergeMaps(base, overlay RawMap) RawMap {
	if base == nil { return overlay }
	out := RawMap{}
	for k, v := range base { out[k] = v }
	for k, ov := range overlay {
		if bm, ok := asMap(out[k]); ok { // asMap accepts RawMap and map[string]any:
			if om, ok2 := asMap(ov); ok2 { // nested maps inherit the parent's named type
				out[k] = mergeMaps(bm, om)
				continue
			}
		}
		out[k] = ov
	}
	return out
}

// overlayTiles merges an env's (or the PR template's) entries onto the base
// tiles: exclusion drops the base tile, a body deep-merges onto it, names only
// in the overlay pass through as-is.
func overlayTiles(base map[string]RawMap, overlay map[string]TileNode) map[string]RawMap {
	out := make(map[string]RawMap, len(base)+len(overlay))
	for name, raw := range base { out[name] = raw }
	for name, node := range overlay {
		if node.Excluded { delete(out, name); continue }
		out[name] = mergeMaps(out[name], node.Raw)
	}
	return out
}

// mergeEnvConf merges an included file's env entry under the main file's.
func mergeEnvConf(base, overlay EnvConf) EnvConf {
	if overlay.Protected { base.Protected = true }
	for k, v := range overlay.Vars {
		if base.Vars == nil { base.Vars = map[string]string{} }
		base.Vars[k] = v
	}
	if base.Tiles == nil { base.Tiles = overlay.Tiles } else {
		for k, v := range overlay.Tiles { base.Tiles[k] = v }
	}
	return base
	// NOTE: an included env's Color, Secrets and Defaults are dropped on the
	// floor here. Same call to make as in Load.
}

// declaredKeys flattens an env overlay to the keys it sets per tile; "env" and
// "limits" flatten one level deeper ("env.PORT"), everything else is the bare
// key. Feeds ResolvedEnv.Declared.
func declaredKeys(overlay map[string]TileNode) map[string]map[string]bool { … }

// mergeSecrets unions the stack-level declarations with an env's own bare
// ones. Options live at stack level only: an env adds names, never redefines
// behavior.
func mergeSecrets(stack SecretsNode, env []string) map[string]SecretConf { … }

// validateVars rejects names that could never be an env variable and names
// already declared as a secret. The two namespaces are separate in a reference
// (${{ stack.vars.X }} vs ${{ stack.secrets.X }}), so one name in both would be
// a row the file contradicts itself about.
// extract: grammar changes per plan: with one params: block the second half
// becomes "one name, one type" inside a collection.
func validateVars(where string, vars map[string]string, secrets map[string]SecretConf) error { … }

// envVarRe is a shell identifier — what an injected name has to be for the
// container to see it at all.
var envVarRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvLines renders a tile's env map as sorted KEY=VALUE lines, the storage
// format the tile row uses.
func EnvLines(env map[string]string) string { … }
```

### Tile validation (`stackconf.go`)

```go
func validateTile(name string, tc TileConf) error {
	// vars, secrets and backups sit where a source slug goes in a reference,
	// so a tile called one of them would make ${{ stack.vars.X }} ambiguous.
	if varref.Reserved(name) {
		return fmt.Errorf("tile %s: %q is reserved for references (${{ stack.%s.NAME }}); pick another name", name, name, name)
	}
	if err := validateBackup(name, tc); err != nil { return err }
	switch tc.UpdatePolicy {
	case "", "off", "notify", "auto": // extract: grammar changes per plan: manual | auto
	default:
		return fmt.Errorf("tile %s: update_policy %q must be off, notify or auto", name, tc.UpdatePolicy)
	}
	switch tc.Type {
	case "service", "cron", "function":
		// Every key is policed against the type that owns it ("port: and
		// domains: don't apply to a cron", "files: is a service key", …) off
		// this one table, so the three run types share one validator. The
		// grammar list above says which key belongs to which type; the checks
		// worth keeping in sight are below.
		pol := runpolicy.Policies[tc.Type] // AllowsIngress / AllowsCommand / RequiresSchedule
		if tc.Build == nil && tc.Image == "" {
			return fmt.Errorf("tile %s: %s needs a build or image source", name, tc.Type)
		}
		if err := validateReplicas(name, tc); err != nil { return err }
		// Line formats are parsed, never just stored: devices, depends_on,
		// files and storage each go through their own parser here, so a typo
		// is a config error instead of a container that won't start.
		…
		if (tc.UpdatePolicy == "notify" || tc.UpdatePolicy == "auto") && tc.Image == "" {
			return fmt.Errorf("tile %s: update_policy watches an image source", name)
		}
		if tc.WaitForCI && tc.Build == nil { return fmt.Errorf("tile %s: wait_for_ci needs a git-built source", name) }
		if pol.RequiresSchedule {
			if tc.Schedule == "" { return fmt.Errorf("tile %s: cron needs a schedule", name) }
			// Validated here, not at create: update never checked, so an invalid
			// schedule landed in the DB and the loader silently skipped the job —
			// a cron that looks configured and never runs.
			if err := jobs.ValidateCron(tc.Schedule); err != nil { return fmt.Errorf("tile %s: %w", name, err) }
			// timeout_minutes is a plain int, so absent and 0 are the same
			// value, both meaning the 30-minute default. Negative is a typo.
			if tc.TimeoutMinutes < 0 { return fmt.Errorf("tile %s: timeout_minutes must be positive", name) }
		} else if tc.Schedule != "" {
			return fmt.Errorf("tile %s: schedule: is a cron key", name)
		}
	case "managed":
		if _, ok := managedtiles.Engines[tc.Engine]; !ok {
			return fmt.Errorf("tile %s: db engine %q is not supported, stackr manages postgres and s3", name, tc.Engine)
		}
		// Caught here rather than at deploy: an out-of-range port reaches
		// docker as a mapping it rejects, and the failure surfaces as a
		// container that won't start rather than as a bad line in the file.
		if tc.ExternalPort < 0 || tc.ExternalPort > 65535 {
			return fmt.Errorf("tile %s: external_port %d out of range", name, tc.ExternalPort)
		}
		if tc.ShmSizeMB < 0 { return fmt.Errorf("tile %s: shm_size_mb must not be negative", name) }
		if !validScope(tc.Scope) { // "" | env | stack | org
			return fmt.Errorf("tile %s: scope %q must be env, stack or org", name, tc.Scope)
		}
		if tc.WaitForCI { return fmt.Errorf("tile %s: wait_for_ci needs a git-built source", name) }
	case "volume":
		if tc.Attach != "" && tc.Path == "" { return fmt.Errorf("tile %s: attached volume needs a path", name) }
	case "slice":
		if tc.From == "" { return fmt.Errorf("tile %s: slice needs a from: instance address", name) }
		if n := strings.Count(tc.From, ".") + 1; n > 4 {
			return fmt.Errorf("tile %s: from %q is not an instance address (instance, stack.instance, org.stack.instance or org.stack.env.instance)", name, tc.From)
		}
		for _, seg := range strings.Split(tc.From, ".") {
			if seg == "" { return fmt.Errorf("tile %s: from %q has an empty segment", name, tc.From) }
		}
		if tc.OnRemove != "" && tc.OnRemove != "keep" && tc.OnRemove != "drop" {
			return fmt.Errorf("tile %s: on_remove %q must be keep or drop", name, tc.OnRemove)
		}
	default:
		return fmt.Errorf("tile %s: unknown type %q", name, tc.Type)
	}
	for _, d := range tc.Domains {
		n := 0
		if d.Host != "" { n++ }
		if d.Apex != "" { n++ }
		if d.Auto { n++ }
		if n != 1 { return fmt.Errorf("tile %s: a domain entry sets exactly one of host, apex or auto", name) }
		if (d.Apex != "" || d.Auto) && (d.Path != "" || d.RedirectTo != "") {
			return fmt.Errorf("tile %s: apex/auto domains take no path or redirect", name)
		}
		// extract: dropped the rule:/middlewares: checks here, raw-proxy keys.
	}
	return nil
}

// validateBackup: dest must be exactly one ${{ …backups.NAME }} reference (a
// literal would be bucket credentials in git); schedule required and a valid
// cron; tz must load; keep >= 0; mode only on a volume backup and one of
// pause|stop|live — on a managed dump it is a plan error rather than a key
// that silently does nothing. Whether the reference resolves to a destination
// this org may use is a PLAN question (it needs the store), not a parse one.
func validateBackup(name string, tc TileConf) error { … }

// validateReplicas refuses replicas above 1 on a tile the pinned classifier
// would call pinned. Refused at parse rather than silently forced back to 1: a
// volume is a directory on one host's disk and one container at a time may
// write it; a config that asks for three and gets one has been lied to about
// what is running.
func validateReplicas(name string, tc TileConf) error {
	if tc.Replicas < 0 { return fmt.Errorf("tile %s: replicas must not be negative", name) }
	if tc.Replicas <= 1 { return nil }
	if tc.Type != "service" { return fmt.Errorf("tile %s: replicas is a service key", name) }
	if len(tc.Volumes) > 0 {
		return fmt.Errorf("tile %s: replicas: %d, but it mounts a volume; a volume is on one machine's disk "+
			"and one container at a time may write it. Drop the volume or set replicas: 1", name, tc.Replicas)
	}
	return nil
}
```

### depends_on: validate and order (`deps.go`)

```go
// validateDeps checks one environment's startup-order graph: every target
// exists in the env, conditions fit the target's kind, no self-deps, no
// cycles. Pure.
func validateDeps(envName string, tiles map[string]TileConf) error {
	for name, tc := range tiles {
		for _, line := range tc.DependsOn {
			slug, cond, err := runtime.ParseDep(line)
			if err != nil { return fmt.Errorf("env %s tile %s: %w", envName, name, err) }
			if slug == name { return fmt.Errorf("env %s tile %s: depends on itself", envName, name) }
			dep, ok := tiles[slug]
			if !ok {
				return fmt.Errorf("env %s tile %s: depends_on %q: no such tile in this environment", envName, name, slug)
			}
			if cond == "completed" {
				if dep.Type != "cron" && dep.Type != "function" {
					return fmt.Errorf("env %s tile %s: depends_on %s:completed; completed only applies to cron and function tiles", envName, name, slug)
				}
				// A function that never runs on deploy can't complete during an
				// apply — that dependency would only ever time out.
				if dep.Type == "function" && !dep.RunOnDeploy {
					return fmt.Errorf("env %s tile %s: depends_on %s:completed needs run_on_deploy: true on %s", envName, name, slug, slug)
				}
			}
		}
	}
	// Cycle check: DFS with white/grey/black colors over the declared edges,
	// tiles visited in sorted order so the reported path is stable. A grey
	// target reports "env X: depends_on cycle through a → b → a".
	…
}

// topoDeps reorders one deploy pass so a tile follows its depends_on targets.
// Kahn with an alphabetical frontier keeps the walk reproducible; edges to
// slugs outside the set (unchanged, already-existing tiles) are ignored; a
// cycle falls back to the incoming order rather than dropping tiles.
func topoDeps(slugs []string, re ResolvedEnv) []string {
	in := map[string]bool{}
	for _, s := range slugs { in[s] = true }
	indeg := map[string]int{}
	dependents := map[string][]string{} // dep -> tiles waiting on it
	for _, s := range slugs { indeg[s] = 0 }
	for _, s := range slugs {
		for _, line := range re.Tiles[s].DependsOn {
			dep, _, err := runtime.ParseDep(line)
			if err != nil || !in[dep] || dep == s { continue }
			indeg[s]++
			dependents[dep] = append(dependents[dep], s)
		}
	}
	frontier := make([]string, 0, len(slugs))
	for _, s := range slugs { if indeg[s] == 0 { frontier = append(frontier, s) } }
	sort.Strings(frontier)
	out := make([]string, 0, len(slugs))
	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		out = append(out, s)
		next := dependents[s]
		sort.Strings(next)
		for _, d := range next {
			indeg[d]--
			if indeg[d] == 0 { frontier = append(frontier, d); sort.Strings(frontier) }
		}
	}
	if len(out) != len(slugs) { return slugs } // cycle, keep incoming order
	return out
}
```

### Plan types (`plan.go`)

```go
// State is a snapshot of what a stack currently looks like in the database.
// Building it (DB reads) happens at the caller; diffing stays pure.
type State struct {
	Envs map[string]EnvState // by slug; ephemeral envs must not be included
	// DomainRes is the stack's OWN domain resources. Org and instance rows are
	// inherited, not owned, so the stack file never deletes them.
	DomainRes []repo.DomainResource
	// AllDomainRes is every resource on the server, for the uniqueness check.
	AllDomainRes []repo.DomainResource
	// OrgConnectors is the connector ids this stack's org owns. A tile's
	// connector: is a raw id from the file and the token it mints clones
	// private repos, so naming another org's id must be a plan error.
	OrgConnectors map[string]bool
	// ConnectorsKnown says the listing above succeeded. A nil map otherwise
	// means two different things — "the lookup failed" and "the org owns none"
	// — and treating both as "do not check" let a file naming another org's
	// connector pass the plan on an org with no connectors of its own.
	ConnectorsKnown bool
	// AllDomains is every tile domain on the server: host+path is unique, so a
	// claim on a host another tile routes must fail in the plan, not as a
	// constraint error mid-apply.
	AllDomains []repo.Domain
	// extract: dropped StackSlug/Middlewares/OrgMiddlewares/MiddlewareUsers,
	// raw-proxy middleware state.
}

type EnvState struct {
	Tiles map[string]TileState
	Color string
	// ApexHosts is the visible resources' bare hosts, so the serializer can
	// round-trip an apex claim as the intent, not a literal.
	ApexHosts map[string]bool
}

type TileState struct {
	Tile    repo.Tile
	Domains []repo.Domain
	// Slice marks this entry as a live provisioned slice rather than a tile
	// row — slices share the tile slug namespace. Resolved when the snapshot
	// is built, because turning provision rows back into addresses takes store
	// lookups and Diff is pure.
	Slice *SliceState
}

type SliceState struct {
	Instance     string // providing instance's slug (identity compare)
	Engine       string // the instance's engine: its SliceName rule normalises the file's name
	InstancePath string // dotted display form, relative where possible
	Name         string // db/bucket name inside the instance
	OnRemove     string // row policy: "" | detach | drop
	Public       bool
}

// Change is one line of a plan.
type Change struct {
	Kind  string `json:"kind"` // create-env | update-env | delete-env | create | delete | update
	Env   string `json:"env"`
	Tile  string `json:"tile,omitempty"`
	Field string `json:"field,omitempty"` // update only
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
	// Note is a consequence the approver has to see before applying: the data
	// loss a db-engine replace causes, a narrowed scope, a dropped slice.
	Note string `json:"note,omitempty"`
	// Fields is what a create row is creating, for the plan page's accordion.
	// Names only, never values — the plan is stored in the database and a
	// resolved tile carries secrets.
	Fields []Field `json:"fields,omitempty"`
	Input  bool    `json:"input,omitempty"` // a value the config declares and nobody has set
	// Destroys marks a change that destroys data without deleting a tile —
	// today only a slice entry leaving the file with on_remove: drop. It
	// exists so the gate can key on consequence rather than on Kind, which
	// would otherwise wave a slice drop through the auto path.
	Destroys bool `json:"destroys,omitempty"`
}

type Field struct{ Name, Value string }

// Input is a value the config declares and nobody has set. Not an error: the
// file says the name exists, the apply makes it exist, and the plan page
// offers a box to fill it in. Blocking instead would mean a stack could never
// be stood up from its own config, which is the point of declaring it there.
type Input struct {
	Scope    string `json:"scope"` // stack | org
	Name     string `json:"name"`
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Blocked is the blast radius as "env/tile": the tiles that read the value
	// plus everything downstream. Empty for org-scope inputs — their readers
	// live in stacks this plan cannot see.
	Blocked []string `json:"blocked,omitempty"`
}

type Plan struct {
	Changes []Change `json:"changes"`
	Errors  []string `json:"errors,omitempty"`
	// Warnings do not block. A declared secret with no value is the case they
	// exist for: the config is legitimate, the stack simply is not configured
	// yet. The deploy of a tile that reads the missing value still fails — an
	// unresolved reference is never run.
	Warnings []string `json:"warnings,omitempty"`
	// GenSecrets is the `default: generated` secrets that still have no value.
	// Minting them is real work, so they make a plan non-empty: a commit whose
	// only change was a new generated secret used to plan "clean", which the
	// auto path skips, so the secret sat unminted until some unrelated change
	// dragged an apply along.
	GenSecrets []string `json:"gen_secrets,omitempty"`
	// hostOwner is host+path → tile id for every domain on the server, plus
	// the claims this plan makes as it goes, so two tiles in one file cannot
	// both take a host.
	hostOwner map[string]string
	Inputs    []Input `json:"inputs,omitempty"`
	// extract: dropped Moves/MoveBlock (pinned-tile node-group move blocks),
	// belongs with node placement if node groups come back.
}

// DiffOpts carries stack-level context the diff needs.
type DiffOpts struct {
	GitURL        string // the bound repo's clone URL, expected on built tiles
	Connector     string // the bound git connector id, fallback for tiles that don't name one
	DefaultBranch string // the bound branch, tile branch fallback
	// Domain-claim context (auto/apex): the slugs that feed generated names
	// and the resources this stack may claim under, nearest level first.
	OrgSlug         string
	StackSlug       string
	DefaultEnv      string
	DomainResources []repo.DomainResource
	// ForeignOrgSlugs is every other organization's slug on this server. A
	// literal host may not start with one: config as code would otherwise be
	// the hole in the anti-squat rule the UI paths enforce.
	ForeignOrgSlugs map[string]bool
	// DNSProvider is the configured ACME DNS-01 provider, "" for none. A
	// wildcard domain needs one, and the plan is where that has to be said:
	// without it the apply writes a hostname whose certificate never issues,
	// and nothing reports why.
	DNSProvider string
	OnlyEnv     string          // restrict the diff to one environment
	SkipEnvs    map[string]bool // envs managed by their own branch's plans
}

func (o DiffOpts) OutOfScope(envSlug string) bool {
	if o.OnlyEnv != "" && envSlug != o.OnlyEnv { return true }
	return o.SkipEnvs[envSlug]
}
```

### Diff (`plan.go`)

```go
// claimHost resolves a domain entry to its concrete hostname.
func claimHost(dc DomainConf, envSlug, tileSlug string, opts DiffOpts) (string, error) {
	switch {
	case dc.Auto:
		if len(opts.DomainResources) == 0 {
			return "", fmt.Errorf("tile %s: auto domain, but no domain resource is visible to this stack; add one at stack, org or server level", tileSlug)
		}
		res := opts.DomainResources[0]
		return service.AutoHost(res, opts.OrgSlug, opts.StackSlug, envSlug, tileSlug, envSlug == opts.DefaultEnv), nil
	case dc.Apex != "":
		for _, r := range opts.DomainResources {
			if r.Host == dc.Apex { return r.Host, nil }
		}
		return "", fmt.Errorf("tile %s: apex %q is not a domain resource visible to this stack", tileSlug, dc.Apex)
	}
	if label, _, ok := strings.Cut(strings.TrimPrefix(dc.Host, "*."), "."); ok && opts.ForeignOrgSlugs[label] {
		return "", fmt.Errorf("tile %s: host %q starts with another organization's slug", tileSlug, dc.Host)
	}
	return dc.Host, nil
}

// releasedHosts is the set of "<tile id> <host+path>" rows this plan takes
// away, so a hostname moving from one tile to another is one change instead of
// a conflict with itself.
//
// Deliberately a list of what GOES, not of what stays: AllDomains is every
// domain row on the server, other stacks included, and a row this plan cannot
// see is a row that is not moving — it has to keep blocking. Only envs the
// plan covers count, and only tiles the file still declares. A tile the file
// has dropped keeps its rows here even though the apply deletes it, which is
// the conservative direction: at worst a plan that both deletes a tile and
// gives its hostname to another one needs two applies.
func releasedHosts(r *Resolved, s State, opts DiffOpts) map[string]bool { … }

// withFileDefault points opts at the FILE's bottom rung rather than whatever
// env the store happens to list first. The store's first row on a hand-made
// stack is the wizard's "Production" even when the file declares another env
// first. The default env is the one whose tiles get the bare generated
// hostname, so getting it wrong makes the wrong env claim
// site.<stack>.<org>.<domain> and every later plan then fails with "already
// routes to another service". opts is a value, so every caller needs it.
func withFileDefault(r *Resolved, opts DiffOpts) DiffOpts { … }

// Diff computes the plan: desired (resolved config) vs current (state).
func Diff(r *Resolved, s State, opts DiffOpts) *Plan {
	opts = withFileDefault(r, opts) // idempotent
	p := &Plan{hostOwner: map[string]string{}}
	// Seed host ownership from what exists, MINUS the rows this plan releases.
	// A host moving between two tiles the same plan touches is one change, not
	// a conflict. Seeding every existing row unconditionally made it a
	// conflict, and a stack in that state had no way forward in the product.
	gone := releasedHosts(r, s, opts)
	for _, d := range s.AllDomains {
		key := d.Host + normPath(d.Path)
		if gone[d.TileID+" "+key] { continue }
		p.hostOwner[key] = d.TileID
	}

	// The home (shared tiles) goes with the stack-scoped plan: it has no rung,
	// so no env-scoped plan carries it. Its row is the store's to create,
	// never a plan change.
	envs := r.EnvOrder
	if _, ok := r.Envs[repo.HomeSlug]; ok { envs = append([]string{repo.HomeSlug}, envs...) }
	for _, envName := range envs {
		if opts.OutOfScope(envName) { continue }
		re := r.Envs[envName]
		cur, envExists := s.Envs[envName]
		switch {
		case envName == repo.HomeSlug: // no env row to create or colour
		case !envExists:
			p.Changes = append(p.Changes, Change{Kind: "create-env", Env: envName})
		case re.Color != cur.Color:
			p.Changes = append(p.Changes, Change{Kind: "update-env", Env: envName, Field: "color",
				Old: defStr(cur.Color, "default"), New: defStr(re.Color, "default")})
		}
		for _, tileName := range sortedTileNames(re.Tiles) {
			tc := re.Tiles[tileName]
			p.checkConnector(envName, tileName, tc, s)
			ts, ok := cur.Tiles[tileName]
			if !ok {
				p.claimHosts(envName, tileName, tc, "", opts)
				p.Changes = append(p.Changes, Change{Kind: "create", Env: envName, Tile: tileName,
					New: tc.Type, Fields: createFields(tc)})
				continue
			}
			p.diffTile(envName, tileName, tc, ts, cur, opts)
		}
		// In the env but absent from config: strict mode deletes.
		for _, tileName := range sortedStateNames(cur.Tiles) {
			if _, declared := re.Tiles[tileName]; declared { continue }
			ch := Change{Kind: "delete", Env: envName, Tile: tileName}
			if sl := cur.Tiles[tileName].Slice; sl != nil {
				if sl.OnRemove == "drop" {
					ch.Destroys = true
					ch.Note = "on_remove: drop. The data behind this slice is destroyed, not orphaned"
				} else {
					ch.Note = "the slice is orphaned and its data kept; drop it from the instance's panel to reclaim the space"
				}
			}
			p.Changes = append(p.Changes, ch)
		}
	}
	if opts.OnlyEnv == "" {
		p.diffDomainRes(r.Domains, s)
		// Static envs not declared: strict mode deletes. Env-scoped plans
		// never judge other envs' existence.
		for _, envName := range sortedEnvNames(s.Envs) {
			if opts.SkipEnvs[envName] || envName == repo.HomeSlug { continue }
			if _, declared := r.Envs[envName]; !declared {
				p.Changes = append(p.Changes, Change{Kind: "delete-env", Env: envName})
			}
		}
	}
	return p
	// extract: dropped planMoves/renameState (moved: is cut) and
	// diffMiddlewares/checkMiddlewareRefs (raw proxy middlewares are cut).
}

// desiredKind maps a config type onto the tile Kind column: cron, function and
// volume keep their name, everything else is a service.
func desiredKind(t string) string { … }

func (p *Plan) diffTile(env, name string, tc TileConf, ts TileState, cur EnvState, opts DiffOpts) {
	t := ts.Tile

	// Slices diff on their own axis; a slice and a tile trading a slug is a
	// replace like any other type change.
	if tc.Type == "slice" || ts.Slice != nil { p.diffSlice(env, name, tc, ts); return }

	// Type change is a replace.
	if t.Kind != desiredKind(tc.Type) || (tc.Type == "managed") != t.IsManaged() {
		p.Changes = append(p.Changes,
			Change{Kind: "delete", Env: env, Tile: name},
			Change{Kind: "create", Env: env, Tile: name, New: tc.Type, Fields: createFields(tc)})
		return
	}
	// A db engine change is a replace too: writing the column left a running
	// postgres labelled mysql. Replace mints a new tile id and volume names
	// derive from it, so the old data is orphaned, not migrated — say so.
	if tc.Type == "managed" && tc.Engine != "" && t.Engine != tc.Engine {
		note := "engine " + t.Engine + " → " + tc.Engine + " replaces the database; its data volume is left behind, not migrated"
		p.Changes = append(p.Changes,
			Change{Kind: "delete", Env: env, Tile: name, Note: note},
			Change{Kind: "create", Env: env, Tile: name, New: tc.Type, Note: note, Fields: createFields(tc)})
		return
	}

	upd := func(field, old, new_ string) {
		if old != new_ {
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name, Field: field, Old: old, New: new_})
		}
	}

	switch tc.Type {
	case "volume":
		upd("attach", attachedSlug(cur, t.AttachedTileID), tc.Attach) // id → slug within the env snapshot
		upd("path", …); upd("volume_name", …); upd("max_size_mb", …)
	case "managed":
		// engine is the replace above. The rest is in-place: the port changes
		// the host mapping, the scope changes who may provision. Both redeploy.
		upd("external_port", …); upd("shm_size_mb", …)
		upd("replicas", strconv.Itoa(max(t.Replicas, 1)), strconv.Itoa(max(tc.Replicas, 1)))
		upd("node_group", t.NodeGroup, tc.NodeGroup)
		// Absent image: means the engine default, not "leave the pin alone",
		// otherwise an override could never be taken back out of the file.
		upd("image", defStr(t.ImageRef, engineDefault(t.Engine)), defStr(tc.Image, engineDefault(t.Engine)))
		if old, want := defStr(t.ScopeKind, "env"), defStr(tc.Scope, "env"); old != want {
			ch := Change{Kind: "update", Env: env, Tile: name, Field: "scope", Old: old, New: want}
			// Narrowing strands whatever is already provisioned from outside
			// the new scope: those tiles keep their credentials and lose the
			// right to be re-provisioned. Worth saying before someone approves.
			if scopeRank(want) < scopeRank(old) { // env 0 < stack 1 < org 2
				ch.Note = "narrowing scope: tiles outside " + want + " can no longer provision from this instance"
			}
			p.Changes = append(p.Changes, ch)
		}
	case "cron", "function":
		diffSource(upd, t, tc, opts)
		// cron: schedule. function: run_on_deploy. Both: command,
		// allow_overlap, timeout_minutes.
		…
		// Absent timeout_minutes means the 30-minute default, not "leave as
		// is", otherwise a value set once could never be taken back out.
		upd("timeout_minutes", strconv.Itoa(t.TimeoutMinutes), strconv.Itoa(defInt(tc.TimeoutMinutes, 30)))
	case "service":
		diffSource(upd, t, tc, opts)
		// One upd() per service key (the grammar list above names them all):
		// scalars compared as strings, list keys through joinNorm.
		upd("replicas", strconv.Itoa(max(t.Replicas, 1)), strconv.Itoa(max(tc.Replicas, 1)))
		// normRestart folds the old "always"/"unless-stopped" spellings into
		// the canonical "" so a row written earlier does not diff forever
		// against a config that says the same thing.
		upd("restart", normRestart(t.RestartPolicy), normRestart(tc.Restart))
		…
		// extract: dropped upd("traefik_override", …), cut key.
	}

	// joinNorm sorts, right for deps, which are a set (unlike domains, where
	// position picks the primary).
	switch tc.Type {
	case "service", "cron", "function":
		upd("depends_on", joinNorm(…), joinNorm(tc.DependsOn))
		upd("wait_for_ci", …)
	}
	switch tc.Type {
	case "service", "cron", "function", "managed":
		upd("update_policy", defStr(t.UpdatePolicy, "off"), defStr(tc.UpdatePolicy, "off"))
	}

	// Absent limits: means "no limits", not "don't touch" — a one-way ratchet
	// otherwise: once set in the file, removing the block never cleared them.
	var wantCPU float64
	var wantMem int
	if tc.Limits != nil { wantCPU, wantMem = tc.Limits.CPU, tc.Limits.MemoryMB }
	upd("cpu_limit", trimFloat(t.CPULimit), trimFloat(wantCPU))
	upd("memory_mb", strconv.Itoa(t.MemLimitMB), strconv.Itoa(wantMem))

	if tc.Env != nil {
		// Values never reach the plan UI: an env value may be a credential, and
		// a plan is rendered in the console and posted into PR comments. The
		// diff is on presence and change, which is what a reviewer needs.
		if old, new_ := canonEnv(t.Env, nil), canonEnv(EnvLines(tc.Env), nil); old != new_ {
			upd("env", "", envChangeSummary(old, new_))
		}
	}
	p.diffDomains(env, name, tc, ts, opts)
	// extract: dropped p.blockOnGroupMove(…) at both node_group sites.
}

// diffSource compares a runnable tile's source (image / git build), shared by
// every run policy, because the base of a runnable type is the same: a source
// the deploy engine turns into an artifact.
func diffSource(upd func(field, old, new_ string), t repo.Tile, tc TileConf, opts DiffOpts) {
	switch {
	case tc.Image != "":
		upd("source", sourceLabel(&t), "image "+tc.Image)
		upd("git_url", t.GitURL, tc.GitURL)
		upd("branch", t.GitBranch, tc.Branch)
		conn := ""
		if tc.GitURL != "" { conn = firstNonEmpty(tc.Connector, opts.Connector) }
		upd("connector", t.ConnectorID, conn)
	default:
		upd("source_type", t.SourceType, "git")
		if u := firstNonEmpty(tc.GitURL, opts.GitURL); u != "" { upd("git_url", t.GitURL, u) }
		upd("connector", t.ConnectorID, firstNonEmpty(tc.Connector, opts.Connector))
		upd("branch", t.GitBranch, firstNonEmpty(tc.Branch, opts.DefaultBranch))
		upd("build_context", normDot(t.BuildContext), normDot(buildContext(tc))) // "." == ""
		upd("dockerfile", defStr(t.DockerfilePath, "Dockerfile"), defStr(dockerfile(tc), "Dockerfile"))
	}
}

// diffSlice compares one slice entry against the live slice under the same
// slug. Identity is (instance, name): moving either is a replace, because the
// data does not follow. on_remove is compared like any other field, and it is
// the HELD policy that decides what a removal does — by then the entry that
// declared it is gone, which is why apply persists it on the provision row.
func (p *Plan) diffSlice(env, name string, tc TileConf, ts TileState) {
	deletion := func() Change {
		ch := Change{Kind: "delete", Env: env, Tile: name}
		if ts.Slice.OnRemove == "drop" {
			ch.Destroys = true
			ch.Note = "on_remove: drop. The data behind this slice is destroyed, not orphaned"
		} else {
			ch.Note = "the slice is orphaned and its data kept; drop it from the instance's panel to reclaim the space"
		}
		return ch
	}
	switch {
	case tc.Type != "slice": // live slice, config says tile: replace
		p.Changes = append(p.Changes, deletion(), Change{Kind: "create", Env: env, Tile: name, New: tc.Type})
		return
	case ts.Slice == nil: // config slice over a live tile: replace; over nothing: create
		if ts.Tile.ID != "" {
			p.Changes = append(p.Changes,
				Change{Kind: "delete", Env: env, Tile: name},
				Change{Kind: "create", Env: env, Tile: name, New: "slice"})
			return
		}
		p.Changes = append(p.Changes, Change{Kind: "create", Env: env, Tile: name, New: "slice",
			Note: "provisions the slice if it does not exist on the instance, adopts it if it does"})
		return
	}
	// Instance identity compares the address's last segment only; two visible
	// instances sharing a slug across scopes would fool it, and the resolver
	// refuses that ambiguity at apply.
	if FromInstance(tc.From) != ts.Slice.Instance ||
		managedtiles.SliceName(ts.Slice.Engine, tc.SliceName(name)) != ts.Slice.Name {
		note := "moving a slice re-provisions it empty; the old data stays behind on " + ts.Slice.Instance
		ch := deletion()
		ch.Note = note
		p.Changes = append(p.Changes, ch, Change{Kind: "create", Env: env, Tile: name, New: "slice", Note: note})
		return
	}
	if removalPolicy(tc.OnRemove) != removalPolicy(ts.Slice.OnRemove) {
		p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
			Field: "on_remove", Old: removalPolicy(ts.Slice.OnRemove), New: removalPolicy(tc.OnRemove)})
	}
	if tc.Public != ts.Slice.Public {
		p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name, Field: "public", Old: …, New: …})
	}
}

func (p *Plan) diffDomains(env, name string, tc TileConf, ts TileState, opts DiffOpts) {
	if tc.Type != "service" { return }
	p.checkDomainRules(env, name, tc, opts)
	cur := map[string]repo.Domain{}
	for _, d := range ts.Domains { cur[domainKey(d.Host, d.Path, d.Rule)] = d }
	seen := map[string]bool{}
	var wantHosts, haveHosts []string
	for _, dc := range tc.Domains {
		host, err := claimHost(dc, env, name, opts)
		if err != nil { p.Errors = append(p.Errors, fmt.Sprintf("env %s: %v", env, err)); continue }
		key := domainKey(host, dc.Path, dc.Rule)
		seen[key] = true
		wantHosts = append(wantHosts, key)
		d, ok := cur[key]
		if !ok {
			if !p.claimHost(env, name, host+normPath(dc.Path), ts.Tile.ID) { continue }
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
				Field: "domain +" + host, New: domainLabel(dc, host)})
			continue
		}
		if want, have := domainLabel(dc, host), domainLabelDB(d); want != have {
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
				Field: "domain " + host, Old: have, New: want})
		}
		if port := domainPort(dc, tc, ts.Tile.ContainerPort, d.ContainerPort); port != d.ContainerPort {
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
				Field: "domain " + host + " port", Old: …, New: …})
		}
	}
	// ts.Domains comes position-ordered; a pure reorder changes the primary
	// (STACKR_PUBLIC_URL) without adding or removing anything.
	for _, d := range ts.Domains { haveHosts = append(haveHosts, domainKey(d.Host, d.Path, d.Rule)) }
	if len(wantHosts) == len(haveHosts) && len(seen) == len(cur) {
		for i := range wantHosts {
			if wantHosts[i] != haveHosts[i] {
				p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
					Field: "domain order", Old: strings.Join(haveHosts, ", "), New: strings.Join(wantHosts, ", "),
					Note: "the first domain is what STACKR_PUBLIC_URL resolves to"})
				break
			}
		}
	}
	for _, key := range sortedKeys(cur) { // removals, sorted for a stable plan
		if seen[key] { continue }
		p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
			Field: "domain -" + cur[key].Host, Old: domainLabelDB(cur[key])})
	}
}

// checkConnector refuses a connector: this stack's org does not own. The value
// is a raw id from the file and the connector behind it mints a token used to
// clone, so without this a file naming another org's id would borrow that
// org's credential and clone its private repositories. A plan error, not an
// apply one: by apply time the deploy is already running. A FAILED lookup
// checks nothing — refusing every build over a database blip is worse than the
// deploy-time guard, which stands either way.
func (p *Plan) checkConnector(env, name string, tc TileConf, s State) {
	if tc.Connector == "" || !s.ConnectorsKnown || s.OrgConnectors[tc.Connector] { return }
	p.Errors = append(p.Errors, fmt.Sprintf("env %s: tile %s: connector %s belongs to another organisation", env, name, tc.Connector))
}

// claimHost records a tile's claim on host+path, or reports it taken when
// another tile (in the database or earlier in this plan) already routes it.
// Ownership ignores rule: a tile may route one host+path several ways, but a
// rule entry on another tile's host would take its traffic by priority.
func (p *Plan) claimHost(env, name, key, ownID string) bool {
	if owner, taken := p.hostOwner[key]; taken && owner != ownID && owner != "plan:"+env+"/"+name {
		p.Errors = append(p.Errors, fmt.Sprintf("env %s: tile %s: %s already routes to another service", env, name, key))
		return false
	}
	p.hostOwner[key] = "plan:" + env + "/" + name
	return true
}

// checkDomainRules applies the domain rules that are not about ownership — the
// ones the domain service applies on the panel and API paths, shared as pure
// functions so the two cannot drift again: a wildcard host with HTTPS needs a
// DNS-01 provider, and a domain's port must resolve. BOTH walks call it: a
// tile the plan creates never reaches diffDomains, and a tile it updates never
// reaches claimHosts.
func (p *Plan) checkDomainRules(env, name string, tc TileConf, opts DiffOpts) { … }

// claimHosts runs the host checks for a tile the plan CREATES: the create path
// never reaches diffDomains, and a taken host there died on the unique index
// mid-apply.
func (p *Plan) claimHosts(env, name string, tc TileConf, ownID string, opts DiffOpts) { … }

// diffDomainRes reconciles the stack's declared domains: with its own rows —
// create when absent (plus an error if any other owner on the server holds the
// host); independent, NOT switched, comparisons for include_env_on_default and
// acme_email, because two fields can move in one edit and a switch showed only
// the first; an "adopted" row when nothing differs but the row was made in the
// panel; delete for a declared row the file dropped. These changes carry Env
// "stack" and the reconcile step takes them out first.
func (p *Plan) diffDomainRes(want []DomainResConf, s State) { … }

// normPath collapses the two spellings of "no path". The web UI stores "/",
// config files omit it, and the proxy treats them identically — but they key
// differently, so a config-declared domain looked missing and every plan
// wanted to re-add a row that was already there.
func normPath(p string) string { if p == "/" { return "" }; return p }

// domainKey is a domain row's identity (the unique host+path index): a rule
// entry can share host and path with a plain one on the same tile. Ownership
// across tiles stays host+path (claimHost).
func domainKey(host, path, rule string) string {
	if rule != "" { return host + normPath(path) + " rule " + rule }
	return host + normPath(path)
}

// domainPort is the container port a declared domain routes to: its own port:,
// else the tile's. A row whose port was only tracking the tile's OLD port
// follows it; anything else set outside the file is left alone when the file
// names no port.
func domainPort(dc DomainConf, tc TileConf, oldTilePort, rowPort int) int {
	switch {
	case dc.Port != 0: return dc.Port
	case tc.Port != 0 && rowPort == oldTilePort: return tc.Port
	}
	return rowPort
}

// canonEnv canonicalizes an env blob (sorted KEY=<quoted value>) so ordering
// and blank lines never show up as diffs. The value is QUOTED, not raw: the
// canonical form is newline-joined, so a multi-line value (a PEM key, a JSON
// blob) used to be indistinguishable from two variables and never round
// tripped to an empty diff.
func canonEnv(raw string, skip map[string]bool) string { … }

// envChangeSummary names which variables change without printing any value:
// "+A +B  ~C  -D", each group sorted.
func envChangeSummary(oldCanon, newCanon string) string { … }

// createFields describes a tile the plan is about to create, in accordion
// order: kind, image, build ("from repo"), git (credentials stripped from the
// URL), branch, port, published ports, hosts, env KEY NAMES ONLY, volumes,
// depends on. A plan is not a place to write secrets down.
func createFields(tc TileConf) []Field { … }

// Destructive: any delete, delete-env, or a row with Destroys.
func (p *Plan) Destructive() bool { … }

func (p *Plan) Empty() bool {
	return len(p.Changes) == 0 && len(p.Errors) == 0 && len(p.GenSecrets) == 0
}

// Declared marks the rows added for values the config declares: one nobody has
// set, one the apply will mint. SHOWN, never COUNTED. A stack whose only row
// is an unset secret has nothing to apply, and counting it left that stack
// with a plan pending review forever.
func (c Change) Declared() bool { return c.Tile == "secrets" && (c.Input || c.New == "generate") }

// InputChange / GenChange render the two declared-value rows (Env carries the
// scope — stack | org — not an env slug); InputsFirst floats inputs above
// generated secrets above everything else, so what the reviewer can act on
// comes before what the apply does on its own.

// Summary renders one line, terraform's phrasing because it works: "3 to add,
// 1 to change, 2 to destroy", plus "N values to set", "N secrets to generate",
// "N errors"; "no changes" when nothing counts.
func (p *Plan) Summary() string { … }
```

Kept but uninteresting one-liners: `defStr`, `defInt`, `firstNonEmpty`,
`trimFloat`, `boolStr`, `normDot`, `scopeRank`, `joinNorm` (trim, drop blanks,
sort, join with `\n`), `sortedTileNames` / `sortedStateNames` /
`sortedEnvNames` / `sortedKeys`, `sourceLabel`, `routeSuffix`, `domainLabel` /
`domainLabelDB`, `attachedSlug`, `redactURL`, `validScope`, `asMap`, `subKeys`,
`sortedMapKeys`, `backupKind`.

---

## File grammar as today

Top level:

- `version:` — int, must be `1`; anything else is refused by name.
- `stack:` — string, required, the stack's slug.
- `include:` — repo-relative paths, merged in order, later wins. Only `shared:`, `base.tiles:`, `vars:` and `environments:` are taken from an include.
- `secrets:` — map of NAME to options (bare NAME = declared, set out of band): `default` (`""` | `generated`), `length` (generated only, 0 = 32), `include_numbers`, `include_symbols`, `required` (plan label only, never a refusal), `env_versions` (default true). Names must be shell identifiers and must not collide with `vars:`. → per plan, a `params:` entry `{type: secret}`.
- `vars:` — map of NAME to plain string, stack-wide, read as `${{ stack.vars.NAME }}`. → per plan, a `params:` entry `{type: param, value}`.
- `defaults:` — cascade rung: `cron_timeout_min`, `cpu_limit`, `mem_limit_mb`, `run_retention_days`, `metric_retention_hours`, `protect`, `protect_user`, `protect_password`, `node_group`. All pointers; half a basic-auth pair is refused.
- `environments:` — a list of names, or a map of name to env body. Order matters: first is the default (bare hostnames). The reserved home slug may not be used.
- `shared:` — name → tile body, stack-scoped, `type: managed` only, lives in the home env, may not also appear under an env.
- `base: tiles:` — name → tile body, copied into every static env.
- `domains:` — list of `{host, acme_email, include_env_on_default}`; host required, unique in the file and server-wide (the latter checked in the plan).
- `pr_envs:` — `{enabled, comment, status, against: [branches], tiles: {…}}`; `tiles` overlays `base.tiles` exactly like an env does.
- `moved:` — rename declarations. **Cut.**
- `ui_edits:` — `block` | `stage`. **Cut.**
- `proxy: middlewares:` — raw traefik bodies, name `[A-Za-z0-9_-]+`, body exactly one middleware type. **Cut** (admin-only, outside the file).

Per environment (map form only):

- `protected:` bool · `color:` palette name or `#rrggbb` · `secrets:` extra names · `defaults:` a cascade rung · `vars:` map shadowing the stack's · `tiles:` name → tile body or `false` (exclusion).
- `apply_policy:` `auto` | `manual`. **Cut** → `from:` (branch name or `promote`) and `auto:` (bool).

Per tile (`type:` = `service` default | `cron` | `function` | `managed` | `volume`; a `from:` makes it a slice):

- Source: `build: {context, dockerfile}` · `image:` · `branch:` · `git_url:` · `connector:` · `watch_paths:` (regex per line, `!` = ignore) · `wait_for_ci:` (needs a build) · `update_policy:` (`off|notify|auto` today → `manual|auto` per plan; needs an image).
- Ingress (service only): `port:` · `domains:` (below) · `security_headers:` · `basic_auth_user:` / `basic_auth_password:` (plain or a `${{ }}` ref) · `published_ports:` · `traefik_override:` (**cut**). Per plan the auth/header keys become the named `proxy:` extras block.
- Runtime: `env:` (map, scalars stringified) · `limits: {cpu, memory_mb}` (absent = no limits, not "don't touch") · `healthcheck:` plus `healthcheck_interval|timeout|retries|start_period` (seconds, ≥ 0) · `user:` · `shm_size_mb:` · `privileged:` · `devices:` (`host[:container[:perms]]`) · `restart:` (`""|always|on-failure|no`) · `command:` · `replicas:` (>1 refused with a volume, service only) · `node_group:`.
- Attachments: `volumes:` (lines) · `files:` (map `repo/path: /container/path[:template]` or lines; service only) · `storage:` (`storage-slug/path-name:/mount[:ro]`; service only) · `depends_on:` (`slug` or `slug:started|healthy|completed`).
- `backup:` — `{dest, schedule, tz, keep, mode, enabled}`; dest must be exactly one `${{ org.backups.NAME }}` / `${{ stackr.backups.NAME }}`; `mode` (pause|stop|live) is volume-only. → per plan it hangs under a `volumes:` entry.
- cron: `schedule:` (required, valid cron) · `timeout_minutes:` (0 = 30) · `allow_overlap:`.
- function: `run_on_deploy:` · `command:` · `timeout_minutes:` · `allow_overlap:`.
- managed: `engine:` (postgres, s3) · `external_port:` (0–65535) · `image:` (absent = engine default) · `shm_size_mb:`; scope is structural, not a key.
- volume: `attach:` (tile slug; needs `path:`) · `path:` · `volume_name:` · `max_size_mb:`.
- slice: `from:` (1–4 dotted segments, none empty) · `name:` (default: the key) · `on_remove:` (`keep` default | `drop`) · `public:` (s3).
- A domain entry: exactly one of `host:` / `apex:` / `auto:`; `path:` and `redirect_to:` only with a literal host; `https:` (nil = true), `force_https:` (nil = true), `priority:`, `port:`; `rule:` and `middlewares:` are raw-proxy keys, **cut**.
- Tile names may not be `vars`, `secrets` or `backups` — those sit where a source slug goes in a reference.

---

## Plan-diff rules (spec)

Scope and walk order:

1. A plan covers the home (shared tiles) first, then the envs in file order. `OnlyEnv` narrows to one env; `SkipEnvs` drops envs managed by their own branch. Env-scoped plans never create or delete other envs, never diff stack-level domains, and never carry the home.
2. The default env for generated hostnames is the FILE's first env, not the store's first row.
3. In the file, not in state → `create-env`. In state, not in the file → `delete-env` (strict mode). Colour difference → `update-env`.

Per tile:

4. Not in state → `create`, with `Fields` listing kind, source, port, hosts, env key NAMES, volumes, deps. Never values.
5. In state, not in the file → `delete` (strict mode). If it is a live slice, the note says orphaned-and-kept, or, with a held `on_remove: drop`, `Destroys: true` and "the data is destroyed".
6. Kind change, managed↔not, or a managed `engine:` change → a `delete` + `create` pair, the engine case noting the data volume is left behind, not migrated.
7. Everything else is a field-by-field `update`, one row per field, old vs new as strings. Empty-means-default is spelled out per field so a key can be taken back OUT of the file: `limits` absent = no limits, `timeout_minutes` 0 = 30, managed `image` absent = engine default, `restart` normalized, build context `"."` = `""`, dockerfile default `Dockerfile`.
8. `env:` diffs as a summary of which keys were added, changed or removed (`+A ~B -C`), never values — the plan is stored and shown in PR comments. Comparison uses a canonical sorted `KEY="quoted value"` form so ordering, blanks and multi-line values are stable.
9. List-valued fields (volumes, watch_paths, devices, files, storage, depends_on, published_ports) compare trimmed, blank-dropped and sorted. Domains do NOT sort: position picks the primary.

Domains:

10. Each entry resolves to a concrete host first: a literal passes through (refused if it starts with another org's slug), `apex:` must name a visible resource, `auto:` generates under the nearest visible resource (error when none is visible).
11. Identity is host+path (+rule when set). Adds are `domain +host`, removals `domain -host`, changes compare a rendered label (path, rule, priority, middlewares, redirect, http / https-no-redirect, `(auto)`).
12. A pure reorder of the same set emits one `domain order` row, noting the first domain is what `STACKR_PUBLIC_URL` resolves to.
13. Ownership: seed host+path → tile id from every domain row on the server, minus the rows THIS plan releases, then claim as the walk goes. A clash with another tile — in the database or earlier in the same plan — is a plan ERROR, because the index is unique server-wide and the insert would otherwise fail mid-apply.
14. The create path runs the same host checks (claims and rules) as the update path; each used to have one and not the other.
15. Non-ownership rules are shared with the UI paths as pure functions: a wildcard host with HTTPS needs a DNS-01 provider, and a domain's port must resolve.

Stack-level `domains:` (full-stack plans only):

16. Absent from state → `create`, plus an error if any other owner on the server already holds the host.
17. `include_env_on_default` and `acme_email` diff independently; an acme change notes that certificates move accounts and the proxy restarts once.
18. A row the file declares that the panel created is ADOPTED, with a note that dropping it from the file will now delete it. Only rows the file created are the file's to delete — otherwise a first apply would drop live hostnames nobody asked to remove.

Slices:

19. Identity is (instance, normalized name). A change to either is delete+create with "re-provisions it empty; the old data stays behind on X".
20. Slice over nothing → `create` that provisions or adopts. Slice over a live tile, or a tile over a live slice → replace.
21. `on_remove` and `public` diff in place. The HELD policy on the row, not the file's, decides what a removal does — by then the declaration is gone.

Connectors:

22. A `connector:` the stack's org does not own is a plan error. A failed lookup checks nothing; "the org owns none" does refuse.

Declared values and counting:

23. An unset declared secret is a row with `Input: true`, never a refusal, carrying the blast radius ("will not deploy: env/tile, …"). A `default: generated` secret with no value is a `generate` row and lands in `GenSecrets`.
24. Those rows are shown but not counted in `Summary` or `Empty`; `GenSecrets` alone makes a plan non-empty, because minting is real work.
25. `Destructive` = any delete, delete-env, or a row with `Destroys`.

Backups:

26. The file owns a tile's whole backup set. Declared, no row → create. Declared, row differs → update. Row, no declaration → delete, noting archives already in the bucket are kept. `enabled: false` keeps the schedule and stops it firing.
27. More than one stored schedule is a removal the plan must SHOW ("the file owns this tile's schedules; N extra one(s) made outside it are removed"): it used to be invisible in the plan and deleted anyway, which is the plan saying one thing and doing another.
28. Kind is derived, never declared: managed → dump, anything else → volume tar.
29. Both sides render through the same summary string (`kind to dest on schedule, keep N[, mode][, disabled]`), so a row appears only when something actually differs.
30. A destination reference that does not resolve is a plan error per tile, not a parse error.
31. An env that does not exist yet: every declared schedule is a create, written once the env exists.

Slice `from:` resolution (needs the store, so it lands in the reconcile step, but the ladder is a rule):

32. `instance` → own env, then stack scope, then org scope; narrowest wins. `stack.instance` → that stack in the current org, else `org.instance`. `org.stack.instance` and `org.stack.env.instance` are exact. No match is an error naming the stack and env it was looked up from.
33. An ephemeral (PR) env never adopts an existing slice: it gets a fresh uniquified copy under the same reference slug, so a preview cannot touch another env's data.
34. Flipping `public` moves every consumer row, not just the representative one: the policy belongs to the bucket.
35. `on_remove` is stamped onto the provision row at apply, because by removal time the declaration that carried it is gone.

---

## Notes for the builder

- Target is `service/internal/flow/promote`: grammar, merge and `Diff` go there as pure functions; the applier becomes promote's reconcile step. `Diff` is already pure — `State` is built by the caller from store reads, and nothing kept here touches a store, Docker, auth or a job queue. Keep it that way: the moment the diff needs a lookup, put it in `State` or `DiffOpts` instead.
- `Load(data, fetch)` takes a fetcher rather than reading files, which is exactly what promote needs: the config comes from the release's commit, not from disk.
- Grammar work the plan requires, all flagged inline: `secrets:`+`vars:` → one `params:` block with collections; `backup:` moves under a `volumes:` entry; the auth/header route keys fold into a named tile `proxy:` block; `update_policy` becomes `manual|auto` (plus `tag_policy`); `apply_policy` becomes env `from:`/`auto:`; `moved:`, `ui_edits:`, `traefik_override:`, `proxy.middlewares:` and domain `rule:`/`middlewares:` all go.
- Env-level `from:` (branch or `promote`) and tile-level `from:` (slice instance address) are different keys at different levels. Do not let one validator see both.
- Two latent bugs worth closing rather than porting: an `include:` silently drops the included file's `secrets:`, `defaults:`, `domains:` and `pr_envs:`, and `mergeEnvConf` silently drops an included env's `color:`, `secrets:` and `defaults:`.
- `topoDeps` (deps.go) is the deploy order the reconcile step wants: Kahn with an alphabetical frontier, edges outside the set ignored, cycle falls back to the incoming order.
- `Resolved.Declared` (which keys an env's own overlay sets, flattened to `env.PORT`) exists so the compare view can mark an intended per-env difference. Cheap to keep, impossible to reconstruct later.
- The home env (shared tiles) is a real key in `Resolved.Envs` and always present, even empty, so a shared tile dropped from the file still diffs as a delete.
- Layering decision to make first: `State`, `EnvState` and `TileState` are typed entirely in `repo.*`, and `resolve`/`Diff` key the home env off `repo.HomeSlug`. No store CALL, but a store DEPENDENCY. Either promote declares its own input structs and the caller maps into them, or promote imports the store package.
- `Plan` is stored and rendered in PR comments: no values, ever. That rule already shaped `createFields`, the env summary and `redactURL`.

Size: source 3283 lines, extract 1637 lines
