package promote

import (
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
)

// DefaultPath is where the stack file lives in the config repo.
const DefaultPath = "stackr-compose.yml"

// File is the stack file as written. Tiles stay raw maps until include and
// env overlays are merged, then decode into TileConf: that is what makes a
// sparse overlay trivial (it overrides only the keys it mentions).
type File struct {
	Version  int                               `yaml:"version"` // must be 1
	Stack    string                            `yaml:"stack"`
	Include  []string                          `yaml:"include"`
	Ladder   []string                          `yaml:"ladder"` // bottom rung first; with head:
	Head     string                            `yaml:"head"`   // the branch the bottom rung builds from
	Params   map[string]map[string]Param       `yaml:"params"`
	Defaults Defaults                          `yaml:"defaults"`
	Domains  []Reservation                     `yaml:"domains"`
	Volumes  map[string]VolumeNode             `yaml:"volumes"`
	Base     struct{ Tiles map[string]RawMap } `yaml:"base"`
	Envs     EnvsNode                          `yaml:"environments"`
	PREnvs   *PREnvs                           `yaml:"pr_envs"`
	Shared   map[string]RawMap                 `yaml:"shared"`
}

// Param is one declaration. A secret is name and type only: the file is in git.
type Param struct {
	Type  string  `yaml:"type"` // param | secret
	Value *string `yaml:"value"`
}

// Defaults is one rung of the settings cascade; nil = say nothing here.
type Defaults struct {
	CPULimit        *float64 `yaml:"cpu_limit" json:"cpu_limit,omitempty"`
	MemLimitMB      *int     `yaml:"mem_limit_mb" json:"mem_limit_mb,omitempty"`
	Protect         *bool    `yaml:"protect" json:"protect,omitempty"`
	ProtectUser     *string  `yaml:"protect_user" json:"protect_user,omitempty"`
	ProtectPassword *string  `yaml:"protect_password" json:"protect_password,omitempty"`
}

// check refuses half a basic-auth pair: a user with no password locks every
// URL below the level behind a password nobody has.
func (d Defaults) check() error {
	if (d.ProtectUser == nil) != (d.ProtectPassword == nil) {
		return fmt.Errorf("protect_user and protect_password go together")
	}
	return nil
}

// Reservation is a stack-level host claim; hosts are unique server-wide.
type Reservation struct {
	Host                string `yaml:"host"`
	ACMEEmail           string `yaml:"acme_email"`
	IncludeEnvOnDefault bool   `yaml:"include_env_on_default"`
}

// VolumeConf is one entry of volumes:. A volume is its own thing, scoped to
// its env; promote never carries one across envs.
type VolumeConf struct {
	MaxSizeMB int         `yaml:"max_size_mb"`
	Backup    *BackupConf `yaml:"backup"`
}

// BackupConf: the destination is a reference, never a literal (bucket
// credentials in git). The kind is not declared: a managed db dumps with
// its engine, a volume tars.
// ponytail: parsed and checked here; promote does not write schedules yet,
// they are set from the panel (flow/backup runs them).
type BackupConf struct {
	Dest     string `yaml:"dest"`
	Schedule string `yaml:"schedule"`
	TZ       string `yaml:"tz"`
	Keep     int    `yaml:"keep"`
	Mode     string `yaml:"mode"` // pause (default) | stop | live
	Enabled  *bool  `yaml:"enabled"`
}

// VolumeNode is a volumes: entry: a body, or false to drop a base volume.
type VolumeNode struct {
	Excluded bool
	Conf     VolumeConf
}

func (v *VolumeNode) UnmarshalYAML(n *yaml.Node) error {
	if excluded(n) {
		v.Excluded = true
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("a volume entry is a map or false")
	}
	return strictNode(n, &v.Conf)
}

// PREnvs is parsed for its shape; PR envs are the connector's (step 4).
type PREnvs struct {
	Enabled *bool               `yaml:"enabled"`
	Against []string            `yaml:"against"`
	Tiles   map[string]TileNode `yaml:"tiles"`
}

// RawMap is a tile body kept unmerged.
type RawMap map[string]any

// EnvsNode accepts a list of names or a map of bodies; order is kept.
type EnvsNode struct {
	Order []string
	Envs  map[string]EnvConf
}

func (e *EnvsNode) UnmarshalYAML(n *yaml.Node) error {
	e.Envs = map[string]EnvConf{}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			e.Order = append(e.Order, c.Value)
			e.Envs[c.Value] = EnvConf{}
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
	return fmt.Errorf("environments is a list of names or a map")
}

// EnvConf is one env's overlay and its two knobs.
type EnvConf struct {
	From     string                `yaml:"from"`   // a branch, or promote
	Branch   string                `yaml:"branch"` // spelling of from: <branch>
	Auto     *bool                 `yaml:"auto"`   // branch envs default on
	Color    string                `yaml:"color"`
	Defaults Defaults              `yaml:"defaults"`
	Tiles    map[string]TileNode   `yaml:"tiles"`
	Volumes  map[string]VolumeNode `yaml:"volumes"`
}

// TileNode is an env entry: an exclusion (false/null) or a body.
type TileNode struct {
	Excluded bool
	Raw      RawMap
}

func (t *TileNode) UnmarshalYAML(n *yaml.Node) error {
	if excluded(n) {
		t.Excluded = true
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("a tile entry is a map or false, got %q", n.Value)
	}
	return n.Decode(&t.Raw)
}

func excluded(n *yaml.Node) bool {
	var b bool
	return n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || (n.Decode(&b) == nil && !b))
}

// TileConf is one merged tile. Kinds are service (a git build), image,
// managed, cron and function; with no type:, engine: makes it managed and a
// lone image: makes it an image tile. kind: is type:'s other spelling. A
// cron or function builds from git like a service unless it names an image.
type TileConf struct {
	Type   string `yaml:"type"`
	Kind   string `yaml:"kind"`
	Image  string `yaml:"image"`
	Engine string `yaml:"engine"`
	Build  *struct {
		Context    string `yaml:"context"`
		Dockerfile string `yaml:"dockerfile"`
	} `yaml:"build"`
	GitURL           string            `yaml:"git_url"` // "" = the config repo
	Branch           string            `yaml:"branch"`  // "" = the config branch
	BuildArgs        map[string]string `yaml:"build_args"`
	WatchPaths       []string          `yaml:"watch_paths"` // regex per line, "!" = ignore
	UpdatePolicy     string            `yaml:"update_policy"`
	TagPolicy        string            `yaml:"tag_policy"`
	Command          string            `yaml:"command"`
	Port             int               `yaml:"port"`
	PublishedPorts   []string          `yaml:"published_ports"`
	EndpointProtocol string            `yaml:"endpoint_protocol"`
	HealthPath       string            `yaml:"health_path"`
	Healthcheck      string            `yaml:"healthcheck"`
	HealthInterval   int               `yaml:"healthcheck_interval"`
	HealthTimeout    int               `yaml:"healthcheck_timeout"`
	HealthRetries    int               `yaml:"healthcheck_retries"`
	HealthStart      int               `yaml:"healthcheck_start_period"`
	Limits           *struct {
		CPU      float64 `yaml:"cpu"`
		MemoryMB int     `yaml:"memory_mb"`
	} `yaml:"limits"`
	User       string       `yaml:"user"`
	ShmSizeMB  int          `yaml:"shm_size_mb"`
	Privileged bool         `yaml:"privileged"`
	Devices    []string     `yaml:"devices"`
	Restart    string       `yaml:"restart"`
	DependsOn  []string     `yaml:"depends_on"`
	Files      []string     `yaml:"files"`
	Volumes    []string     `yaml:"volumes"` // "volume-slug:/path[:ro]"
	Replicas   int          `yaml:"replicas"`
	Env        EnvMap       `yaml:"env"`
	Domains    []DomainConf `yaml:"domains"`
	Slices     []SliceConf  `yaml:"slices"`
	// cron: schedule (a cron expression, CRON_TZ= allowed); function:
	// trigger (manual | on_deploy); both: timeout_minutes (0 = 30).
	Schedule       string `yaml:"schedule"`
	Trigger        string `yaml:"trigger"`
	TimeoutMinutes int    `yaml:"timeout_minutes"`
}

// DomainConf is one claim; the first listed is the primary. Host takes
// params. refs only.
type DomainConf struct {
	Host       string     `yaml:"host"`
	Path       string     `yaml:"path"`
	Port       int        `yaml:"port"` // 0 = the tile's
	HTTPS      *bool      `yaml:"https"`
	ForceHTTPS *bool      `yaml:"force_https"`
	RedirectTo string     `yaml:"redirect_to"`
	Proxy      *ProxyConf `yaml:"proxy"`
}

// ProxyConf is the named, safe-by-construction extras; raw Caddy config is
// admin-only and never in the file.
type ProxyConf struct {
	BasicAuth *struct {
		User     string `yaml:"user"`
		Password string `yaml:"password"` // a ${{ }} ref, hashed when the route is written
	} `yaml:"basic_auth"`
	Websockets bool `yaml:"websockets"`
	MaxBodyMB  int  `yaml:"max_body_mb"`
	Timeouts   *struct {
		Dial  int `yaml:"dial"`
		Read  int `yaml:"read"`
		Write int `yaml:"write"`
	} `yaml:"timeouts"`
	Headers     map[string]string `yaml:"headers"`
	Methods     []string          `yaml:"methods"`
	StripPrefix bool              `yaml:"strip_prefix"`
	SecHeaders  bool              `yaml:"security_headers"`
}

// SliceConf attaches the tile to a managed instance: its own db or bucket.
// From is the instance tile's slug, looked up among the instances visible
// from the env.
type SliceConf struct {
	From     string `yaml:"from"`
	Name     string `yaml:"name"`      // "" = the consumer's slug
	OnRemove string `yaml:"on_remove"` // keep (default) | drop
	Public   bool   `yaml:"public"`
}

func (s *SliceConf) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.From = n.Value
		return nil
	}
	type plain SliceConf
	return strictNode(n, (*plain)(s))
}

// EnvMap stringifies scalars (PORT: 8080 unquoted).
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

// Resolved is the file after includes and overlays.
type Resolved struct {
	Stack    string
	Order    []string // the ladder, bottom rung first
	Params   map[string]map[string]Param
	Defaults Defaults
	Domains  []Reservation
	Envs     map[string]ResolvedEnv
}

type ResolvedEnv struct {
	FromKind, FromBranch string
	Auto                 bool
	Color                string
	Defaults             Defaults
	Tiles                map[string]TileConf
	Volumes              map[string]VolumeConf
}

// Fetcher loads an included file by repo-relative path.
type Fetcher func(path string) ([]byte, error)

// strictYAML decodes refusing unknown keys, top level included.
func strictYAML(data []byte, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && err != io.EOF {
		return humanYAML(err)
	}
	return nil
}

// strictNode is strictYAML for a node under a custom unmarshaler, which does
// not inherit the decoder's KnownFields.
func strictNode(n *yaml.Node, out any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return strictYAML(b, out)
}

// removedKeys tells an old file what replaced a key.
var removedKeys = map[string]string{
	"compose":          "use image: or build:",
	"compose_inline":   "use image: or build:",
	"compose_path":     "use image: or build:",
	"moved":            "drop it; re-adding a slug re-adopts its orphaned volume",
	"ui_edits":         "drop it; drift never promotes",
	"apply_policy":     "use from: and auto: on the environment",
	"traefik_override": "raw proxy config is admin-only",
	"secrets":          "declare them under params: with type: secret",
	"vars":             "declare them under params:",
	"run_on_deploy":    "use trigger: on_deploy on a function tile",
	"allow_overlap":    "runs of one tile never overlap; a run that would is recorded cancelled",
	"basic_auth_user":  "use proxy: basic_auth: on the domain",
	"security_headers": "use proxy: security_headers: on the domain",
	"backup":           "backup: goes under the volume in volumes:",
}

var unknownField = regexp.MustCompile(`^(?:line \d+: )?field (\S+) not found in type \S+$`)

// humanYAML rewrites yaml.v3's unknown-key errors: they name a Go type and
// count lines in a re-marshalled fragment, which means nothing to the
// person editing the file.
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
			s += " (no longer supported: " + hint + ")"
		}
		msgs = append(msgs, s)
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// Parse decodes and checks the top level.
func Parse(data []byte) (*File, error) {
	var f File
	if err := strictYAML(data, &f); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d (want 1)", f.Version)
	}
	if len(f.Shared) > 0 { // DECIDE 30
		return nil, fmt.Errorf("shared: is not supported yet; declare the instance in an environment")
	}
	seen := map[string]bool{}
	for _, d := range f.Domains {
		switch {
		case strings.TrimSpace(d.Host) == "":
			return nil, fmt.Errorf("domains: host required")
		case seen[d.Host]:
			return nil, fmt.Errorf("domains: %s declared twice", d.Host)
		}
		seen[d.Host] = true
	}
	if err := f.Defaults.check(); err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	return &f, nil
}

// Load parses data, merges includes in order (later wins) and resolves the
// env overlays. An include contributes base tiles, volumes, params and
// environments only.
func Load(data []byte, fetch Fetcher) (*Resolved, error) {
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for _, inc := range f.Include {
		if fetch == nil {
			return nil, fmt.Errorf("include %s: no way to fetch includes here", inc)
		}
		b, err := fetch(inc)
		if err != nil {
			return nil, fmt.Errorf("include %s: %w", inc, err)
		}
		var x File
		if err := strictYAML(b, &x); err != nil {
			return nil, fmt.Errorf("include %s: %w", inc, err)
		}
		if f.Base.Tiles == nil {
			f.Base.Tiles = map[string]RawMap{}
		}
		for n, raw := range x.Base.Tiles {
			f.Base.Tiles[n] = mergeMaps(f.Base.Tiles[n], raw)
		}
		if f.Volumes == nil {
			f.Volumes = map[string]VolumeNode{}
		}
		for n, v := range x.Volumes {
			f.Volumes[n] = v
		}
		if f.Params == nil {
			f.Params = map[string]map[string]Param{}
		}
		for c, ps := range x.Params {
			if f.Params[c] == nil {
				f.Params[c] = map[string]Param{}
			}
			for n, p := range ps {
				f.Params[c][n] = p
			}
		}
		if f.Envs.Envs == nil {
			f.Envs.Envs = map[string]EnvConf{}
		}
		for _, n := range x.Envs.Order {
			base, ok := f.Envs.Envs[n]
			if !ok {
				f.Envs.Order = append(f.Envs.Order, n)
			}
			f.Envs.Envs[n] = mergeEnv(base, x.Envs.Envs[n])
		}
	}
	return resolve(f)
}

func resolve(f *File) (*Resolved, error) {
	if f.Stack == "" {
		return nil, fmt.Errorf("stack name required")
	}
	r := &Resolved{
		Stack:    f.Stack,
		Params:   f.Params,
		Defaults: f.Defaults,
		Domains:  f.Domains,
		Envs:     map[string]ResolvedEnv{},
	}
	if err := checkParams(f.Params); err != nil {
		return nil, err
	}
	order, envs := f.Envs.Order, f.Envs.Envs
	if len(f.Ladder) > 0 {
		if len(order) > 0 && !sameSet(order, f.Ladder) {
			return nil, fmt.Errorf("ladder: and environments: name different environments")
		}
		order = f.Ladder
	}
	if len(order) == 0 {
		order = []string{"production"}
	}
	r.Order = order
	for i, name := range order {
		if !slug.Valid(name) {
			return nil, fmt.Errorf("environment %q: a name is lower-case letters, digits and single hyphens", name)
		}
		ec := envs[name]
		if err := ec.Defaults.check(); err != nil {
			return nil, fmt.Errorf("environment %s defaults: %w", name, err)
		}
		re := ResolvedEnv{
			Color:    ec.Color,
			Defaults: ec.Defaults,
			Tiles:    map[string]TileConf{},
			Volumes:  map[string]VolumeConf{},
		}
		if err := knobs(&re, ec, f, i); err != nil {
			return nil, fmt.Errorf("environment %s: %w", name, err)
		}
		for n, raw := range overlayTiles(f.Base.Tiles, ec.Tiles) {
			tc, err := decodeTile(raw)
			if err != nil {
				return nil, fmt.Errorf("environment %s tile %s: %w", name, n, err)
			}
			if err := checkTile(n, tc); err != nil {
				return nil, fmt.Errorf("environment %s: %w", name, err)
			}
			re.Tiles[n] = tc
		}
		for n, v := range overlayVolumes(f.Volumes, ec.Volumes) {
			if err := checkVolume(n, v); err != nil {
				return nil, fmt.Errorf("environment %s: %w", name, err)
			}
			re.Volumes[n] = v
		}
		if err := checkRefs(name, re); err != nil {
			return nil, err
		}
		r.Envs[name] = re
	}
	return r, nil
}

// knobs compiles from/auto. ladder:+head: gives the bottom rung the head
// branch and the rest promote; an env's own from:/branch: wins.
func knobs(re *ResolvedEnv, ec EnvConf, f *File, rung int) error {
	from := ec.From
	switch {
	case ec.Branch != "" && from != "":
		return fmt.Errorf("from: and branch: say the same thing; keep one")
	case ec.Branch != "":
		from = ec.Branch
	case from == "" && len(f.Ladder) > 0 && rung == 0:
		from = f.Head
	case from == "" && len(f.Ladder) > 0:
		from = environment.FromPromote
	}
	switch {
	case from == environment.FromPromote && rung == 0:
		return fmt.Errorf("the bottom rung has nothing below it to promote from; give it a branch")
	case from == environment.FromPromote:
		re.FromKind = environment.FromPromote
	case from != "":
		re.FromKind, re.FromBranch, re.Auto = environment.FromBranch, from, true // branch envs default to auto
	}
	if ec.Auto != nil {
		re.Auto = *ec.Auto
	}
	return nil
}

func decodeTile(raw RawMap) (TileConf, error) {
	var tc TileConf
	b, err := yaml.Marshal(map[string]any(raw))
	if err != nil {
		return tc, err
	}
	if err := strictYAML(b, &tc); err != nil {
		return tc, err
	}
	if tc.Kind != "" {
		if tc.Type != "" && tc.Type != tc.Kind {
			return tc, fmt.Errorf("kind: %s and type: %s disagree; kind: and type: are one key, give one", tc.Kind, tc.Type)
		}
		tc.Type, tc.Kind = tc.Kind, ""
	}
	if tc.Type == "" {
		switch {
		case tc.Engine != "":
			tc.Type = "managed"
		case tc.Image != "" && tc.Build == nil && tc.GitURL == "":
			tc.Type = "image"
		default:
			tc.Type = "service"
		}
	}
	return tc, nil
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkTile is the file-level check; leaf/tile.Validate runs over the row
// the plan builds from it, so the two cannot disagree on a column.
func checkTile(name string, tc TileConf) error {
	if !slug.Valid(name) || slug.Reserved(name) {
		return fmt.Errorf("tile %q: a tile name is lower-case letters, digits and single hyphens, not params", name)
	}
	switch tc.Type {
	case "service", "image":
		if tc.Engine != "" {
			return fmt.Errorf("tile %s: engine: is a managed key", name)
		}
	case "cron", "function":
		if tc.Engine != "" {
			return fmt.Errorf("tile %s: engine: is a managed key", name)
		}
		if len(tc.Domains) > 0 {
			return fmt.Errorf("tile %s: a %s has no endpoint; domains do not apply", name, tc.Type)
		}
	case "managed":
		if tc.Engine == "" {
			return fmt.Errorf("tile %s: a managed tile names its engine", name)
		}
		if len(tc.Domains) > 0 || len(tc.Slices) > 0 {
			return fmt.Errorf("tile %s: a managed instance takes no domains or slices", name)
		}
	default:
		return fmt.Errorf("tile %s: unknown type %q (service, image, managed, cron or function)", name, tc.Type)
	}
	for k := range tc.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("tile %s: env %q is not an env var name", name, k)
		}
	}
	if tc.Replicas > 1 && len(tc.Volumes) > 0 {
		return fmt.Errorf(
			"tile %s: replicas: %d, but it mounts a volume; one writer per volume. Drop the volume or set replicas: 1",
			name,
			tc.Replicas,
		)
	}
	for _, s := range tc.Slices {
		switch {
		case !slug.Valid(s.From):
			return fmt.Errorf("tile %s: slice from %q is not an instance name", name, s.From)
		case s.OnRemove != "" && s.OnRemove != "keep" && s.OnRemove != "drop":
			return fmt.Errorf("tile %s: on_remove %q must be keep or drop", name, s.OnRemove)
		}
	}
	for _, d := range tc.Domains {
		if d.Host == "" {
			return fmt.Errorf("tile %s: a domain needs a host", name)
		}
	}
	return nil
}

func checkVolume(name string, v VolumeConf) error {
	if !slug.Valid(name) {
		return fmt.Errorf("volume %q: a volume name is lower-case letters, digits and single hyphens", name)
	}
	if v.MaxSizeMB < 0 {
		return fmt.Errorf("volume %s: max_size_mb must not be negative", name)
	}
	b := v.Backup
	if b == nil {
		return nil
	}
	switch {
	case !strings.HasPrefix(strings.TrimSpace(b.Dest), "${{") || strings.Count(b.Dest, "${{") != 1:
		return fmt.Errorf("volume %s: backup dest is one ${{ org.backups.NAME }} reference, never a literal", name)
	case strings.TrimSpace(b.Schedule) == "":
		return fmt.Errorf("volume %s: backup needs a schedule", name)
	case b.Keep < 0:
		return fmt.Errorf("volume %s: keep must not be negative", name)
	case b.Mode != "" && b.Mode != "pause" && b.Mode != "stop" && b.Mode != "live":
		return fmt.Errorf("volume %s: mode %q must be pause, stop or live", name, b.Mode)
	}
	return nil
}

// checkRefs: every volume line names a declared volume, depends_on names a
// tile of the env, no cycles.
func checkRefs(env string, re ResolvedEnv) error {
	for _, n := range slices.Sorted(maps.Keys(re.Tiles)) {
		tc := re.Tiles[n]
		for _, l := range tc.Volumes {
			v, _, _ := strings.Cut(l, ":")
			if _, ok := re.Volumes[v]; !ok {
				return fmt.Errorf("environment %s tile %s: volume %q is not declared under volumes", env, n, v)
			}
		}
		for _, d := range tc.DependsOn {
			dep, _, _ := strings.Cut(d, ":")
			switch _, ok := re.Tiles[dep]; {
			case dep == n:
				return fmt.Errorf("environment %s tile %s: depends on itself", env, n)
			case !ok:
				return fmt.Errorf("environment %s tile %s: depends_on %q: no such tile in this environment", env, n, dep)
			}
		}
	}
	if order := topo(slices.Sorted(maps.Keys(re.Tiles)), re.Tiles); len(order) == 0 && len(re.Tiles) > 0 {
		return fmt.Errorf("environment %s: depends_on has a cycle", env)
	}
	return nil
}

func checkParams(ps map[string]map[string]Param) error {
	for c, entries := range ps {
		if !slug.ValidName(c) {
			return fmt.Errorf("params: collection %q is lower-case letters, digits and _", c)
		}
		for n, p := range entries {
			switch {
			case !slug.ValidName(n):
				return fmt.Errorf("params: %s.%s: a name is lower-case letters, digits and _", c, n)
			case p.Type == "secret" && p.Value != nil:
				return fmt.Errorf("params: %s.%s is a secret; its value never goes in the file", c, n)
			case p.Type != "secret" && p.Type != "param":
				return fmt.Errorf("params: %s.%s: type must be param or secret", c, n)
			}
		}
	}
	return nil
}

// topo orders slugs so a tile follows its depends_on targets (Kahn with an
// alphabetical frontier, so it is reproducible). nil on a cycle.
func topo(slugs []string, tiles map[string]TileConf) []string {
	in := map[string]bool{}
	for _, s := range slugs {
		in[s] = true
	}
	indeg, next := map[string]int{}, map[string][]string{}
	for _, s := range slugs {
		for _, d := range tiles[s].DependsOn {
			dep, _, _ := strings.Cut(d, ":")
			if in[dep] && dep != s {
				indeg[s]++
				next[dep] = append(next[dep], s)
			}
		}
	}
	var frontier, out []string
	for _, s := range slugs {
		if indeg[s] == 0 {
			frontier = append(frontier, s)
		}
	}
	for len(frontier) > 0 {
		sort.Strings(frontier)
		s := frontier[0]
		frontier = frontier[1:]
		out = append(out, s)
		for _, d := range next[s] {
			if indeg[d]--; indeg[d] == 0 {
				frontier = append(frontier, d)
			}
		}
	}
	if len(out) != len(slugs) {
		return nil
	}
	return out
}

// mergeMaps deep-merges overlay onto base: maps merge, anything else
// (lists included) replaces wholesale.
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
			if om, ok := asMap(ov); ok {
				out[k] = mergeMaps(bm, om)
				continue
			}
		}
		out[k] = ov
	}
	return out
}

func asMap(v any) (RawMap, bool) {
	switch m := v.(type) {
	case RawMap:
		return m, true
	case map[string]any:
		return RawMap(m), true
	}
	return nil, false
}

func overlayTiles(base map[string]RawMap, overlay map[string]TileNode) map[string]RawMap {
	out := map[string]RawMap{}
	for n, raw := range base {
		out[n] = raw
	}
	for n, node := range overlay {
		if node.Excluded {
			delete(out, n)
			continue
		}
		out[n] = mergeMaps(out[n], node.Raw)
	}
	return out
}

func overlayVolumes(base, overlay map[string]VolumeNode) map[string]VolumeConf {
	out := map[string]VolumeConf{}
	for n, v := range base {
		if !v.Excluded {
			out[n] = v.Conf
		}
	}
	for n, v := range overlay {
		if v.Excluded {
			delete(out, n)
			continue
		}
		out[n] = v.Conf
	}
	return out
}

// mergeEnv merges an included file's env under the main file's: the main
// file's knobs win, tiles and volumes add.
func mergeEnv(base, x EnvConf) EnvConf {
	if base.From == "" && base.Branch == "" {
		base.From, base.Branch = x.From, x.Branch
	}
	if base.Auto == nil {
		base.Auto = x.Auto
	}
	if base.Color == "" {
		base.Color = x.Color
	}
	if base.Tiles == nil {
		base.Tiles = map[string]TileNode{}
	}
	for k, v := range x.Tiles {
		if _, ok := base.Tiles[k]; !ok {
			base.Tiles[k] = v
		}
	}
	if base.Volumes == nil {
		base.Volumes = map[string]VolumeNode{}
	}
	for k, v := range x.Volumes {
		if _, ok := base.Volumes[k]; !ok {
			base.Volumes[k] = v
		}
	}
	return base
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		if !m[s] {
			return false
		}
	}
	return true
}
