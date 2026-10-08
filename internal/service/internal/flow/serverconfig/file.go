package serverconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
)

// File is the server file as written. A nil block says nothing about its
// subject; a present one (even empty) is the whole list, so what the file
// no longer names becomes a removal row. The omitempty tags are for export.
type File struct {
	Version     int                         `yaml:"version"`            // must be 1
	Settings    map[string]any              `yaml:"settings,omitempty"` // Flat knobs by key; a key left out is not touched
	Defaults    *Defaults                   `yaml:"defaults,omitempty"` // the server rung of the cascade
	Params      map[string]map[string]Param `yaml:"params,omitempty"`   // server scope; a secret by name only
	Routes      []Route                     `yaml:"routes,omitempty"`
	BackupDests []Dest                      `yaml:"backup_dests,omitempty"`
	Domains     []Domain                    `yaml:"domains,omitempty"` // instance domain resources
	Connectors  []Connector                 `yaml:"connectors,omitempty"`
	Orgs        []Org                       `yaml:"orgs,omitempty"`
}

// setting is a settings: value as text; Parse leaves every value a string.
func (f *File) setting(key string) (string, bool) {
	v, ok := f.Settings[key].(string)
	return v, ok
}

// Param is one declaration, the other files' grammar.
type Param = planfile.Param

// Defaults is the server rung of the settings cascade: the same fields as
// leaf/settings.Settings (the conversion below relies on it), with yaml
// keys. ponytail: orgconfig has the same struct, and flows do not import
// flows; the third copy moves into planfile.
type Defaults struct {
	CPULimit        *float64 `yaml:"cpu_limit,omitempty" json:"cpu_limit,omitempty"`
	MemLimitMB      *int     `yaml:"mem_limit_mb,omitempty" json:"mem_limit_mb,omitempty"`
	Protect         *bool    `yaml:"protect,omitempty" json:"protect,omitempty"`
	ProtectUser     *string  `yaml:"protect_user,omitempty" json:"protect_user,omitempty"`
	ProtectPassword *string  `yaml:"protect_password,omitempty" json:"protect_password,omitempty"`
}

// Route is an external route: a host that goes to an address outside stackr.
type Route struct {
	Host     string `yaml:"host"`
	Mode     string `yaml:"mode"` // passthrough | http | https
	Target   string `yaml:"target"`
	Insecure bool   `yaml:"insecure,omitempty"`
}

// Dest is a global S3 backup destination. The three keys are
// ${{ server.params.<collection>.<name> }} refs: the file is in git.
// ArchiveKey is optional and only applies when the destination is created.
type Dest struct {
	Name       string `yaml:"name"`
	Kind       string `yaml:"kind,omitempty"` // s3 (the only one a file declares)
	Endpoint   string `yaml:"endpoint,omitempty"`
	Region     string `yaml:"region,omitempty"`
	Bucket     string `yaml:"bucket"`
	AccessKey  string `yaml:"access_key"`
	SecretKey  string `yaml:"secret_key"`
	ArchiveKey string `yaml:"archive_key,omitempty"`
	Shared     bool   `yaml:"shared,omitempty"` // every org may pick it
}

// Domain is an instance domain resource. From renames the resource that has
// that host: its hosts move with it.
type Domain struct {
	Host                string `yaml:"host"`
	From                string `yaml:"from,omitempty"`
	IncludeEnvOnDefault bool   `yaml:"include_env_on_default"`
	ACMEEmail           string `yaml:"acme_email,omitempty"`
}

// Connector names a server connector the file shares. It never creates one
// (a GitHub App is made in the panel's browser flow). A nil Share says
// nothing about the shares.
type Connector struct {
	Name  string `yaml:"name"`
	Share *Share `yaml:"share,omitempty"`
}

// Share is `all` or a list of org slugs; an empty list is shared with none.
type Share struct {
	All  bool
	Orgs []string
}

func (s *Share) UnmarshalYAML(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.ScalarNode && n.Value == "all":
		s.All = true
		return nil
	case n.Kind == yaml.SequenceNode:
		s.Orgs = []string{}
		return n.Decode(&s.Orgs)
	}
	return fmt.Errorf("line %d: share is all, or a list of org slugs", n.Line)
}

func (s Share) MarshalYAML() (any, error) {
	if s.All {
		return "all", nil
	}
	return s.Orgs, nil
}

// Org is an org the file creates (when it does not exist) and binds to its
// own org file. Connector names a server connector. Name is only used to
// create the org; once it exists the org file renames it.
type Org struct {
	Slug      string `yaml:"slug"`
	Name      string `yaml:"name,omitempty"` // "" = the slug
	Repo      string `yaml:"repo,omitempty"`
	Branch    string `yaml:"branch,omitempty"`
	Path      string `yaml:"path,omitempty"`
	Connector string `yaml:"connector,omitempty"`
	Auto      bool   `yaml:"auto,omitempty"`
}

// OrgFilePath is where an org file lives when a binding names no path
// (flow/orgconfig.DefaultPath; flows do not import flows).
const OrgFilePath = "stackr-org.yml"

// removedKeys tells an old file what replaced a key; the server file is new,
// so there are none.
var removedKeys map[string]string

// Parse decodes and checks the whole file; Diff trusts what it returns.
// Values it can normalise (hosts, repos, targets, setting values) are
// normalised in place, so Diff compares like with like.
func Parse(data []byte) (*File, error) {
	var f File
	if err := planfile.StrictYAML(data, &f, removedKeys); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d (want 1)", f.Version)
	}
	for _, check := range []func() error{
		f.checkSettings,
		f.checkDefaults,
		func() error { return planfile.CheckParams(f.Params) },
		f.checkRoutes,
		f.checkDests,
		f.checkDomains,
		f.checkConnectors,
		f.checkOrgs,
	} {
		if err := check(); err != nil {
			return nil, err
		}
	}
	return &f, nil
}

// ---- settings ----

func (f *File) checkSettings() error {
	for key, v := range f.Settings {
		k, ok := knob(key)
		switch {
		case !ok:
			return fmt.Errorf("settings: %q is not an install setting", key)
		case k.ConfigOnly:
			return fmt.Errorf("settings.%s is the server file's own binding; set it on the Config tab or with stackr server bind", key)
		case k.ReadOnly:
			return fmt.Errorf("settings.%s comes from the installer and cannot be set here", key)
		}
		raw, err := scalar(v)
		if err != nil {
			return fmt.Errorf("settings.%s: %w", key, err)
		}
		if raw, err = CheckSetting(key, raw); err != nil {
			return fmt.Errorf("settings.%s: %w", key, err)
		}
		f.Settings[key] = raw
	}
	return nil
}

// scalar reads a settings value as text. null and "" both reset the knob
// to its default.
func scalar(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(x), nil
	case bool, int, int64, float64:
		return fmt.Sprint(x), nil
	}
	return "", fmt.Errorf("give a single value, not a list or a map")
}

// knob is a Flat catalogue entry.
func knob(key string) (settings.Knob, bool) {
	i := slices.IndexFunc(settings.Catalogue, func(k settings.Knob) bool { return k.Key == key && k.Scopes == settings.Flat })
	if i < 0 {
		return settings.Knob{}, false
	}
	return settings.Catalogue[i], true
}

// CheckSetting is settings.Check plus what SetSettings and the proxy refuse
// later; it returns the value as the diff compares it.
func CheckSetting(key, raw string) (string, error) {
	if err := settings.Check(key, raw); err != nil {
		return "", err
	}
	if raw == "" {
		return "", nil
	}
	switch key {
	case "panel_domain", "root_domain":
		host, err := installspec.CheckRoot(raw)
		if err != nil {
			return "", err
		}
		if key == "panel_domain" && strings.HasPrefix(host, "*.") {
			return "", fmt.Errorf("the panel answers on one name, not a wildcard")
		}
		return host, nil
	case "acme_email":
		if a, err := mail.ParseAddress(raw); err != nil || a.Address != raw {
			return "", fmt.Errorf("%s is not an email address", raw)
		}
	case "dns_provider":
		// ponytail: cloudflare is the one provider compiled into the proxy.
		if raw != "cloudflare" {
			return "", fmt.Errorf("%q is not a provider in this build (cloudflare)", raw)
		}
	case "trusted_proxies":
		for _, r := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			if _, err := netip.ParsePrefix(r); err != nil {
				if _, err := netip.ParseAddr(r); err != nil {
					return "", fmt.Errorf("%q is not an IP or CIDR", r)
				}
			}
		}
	case "proxy_custom":
		// ponytail: shape only (a JSON array of Caddy route objects); Caddy's load is the real check.
		var routes []json.RawMessage
		if err := json.Unmarshal([]byte(raw), &routes); err != nil {
			return "", fmt.Errorf("a JSON array of Caddy route objects: %w", err)
		}
	}
	return raw, nil
}

func (f *File) checkDefaults() error {
	d := f.Defaults
	if d == nil {
		return nil
	}
	if (d.CPULimit != nil && *d.CPULimit < 0) || (d.MemLimitMB != nil && *d.MemLimitMB < 0) {
		return fmt.Errorf("defaults: a limit is not negative")
	}
	if err := settings.Settings(*d).Check(); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	return nil
}

// ---- routes ----

var routeModes = []string{"passthrough", "http", "https"}

func (f *File) checkRoutes() error {
	seen := map[string]bool{}
	for i, r := range f.Routes {
		if strings.ContainsAny(r.Host, "/: ") {
			return fmt.Errorf("routes: %q is not a bare hostname", r.Host)
		}
		host, err := installspec.CheckRoot(r.Host)
		switch {
		case err != nil:
			return fmt.Errorf("routes: %s: %w", r.Host, err)
		case seen[host]:
			return fmt.Errorf("routes: %s declared twice", host)
		case !slices.Contains(routeModes, r.Mode):
			return fmt.Errorf("routes: %s: mode is passthrough, http or https", host)
		case r.Insecure && r.Mode != "https":
			return fmt.Errorf("routes: %s: insecure only applies to an https route", host)
		}
		seen[host] = true
		target, err := NormTarget(r.Target, r.Mode)
		if err != nil {
			return fmt.Errorf("routes: %s: %w", host, err)
		}
		f.Routes[i].Host, f.Routes[i].Target = host, target
	}
	return nil
}

// NormTarget is host:port with the mode's default port filled in, the way
// leaf/route stores it. ponytail: the same rules as leaf/route's checkTarget
// (a leaf is not importable from a flow's parse, and it is unexported);
// dedupe when route exports a Check.
func NormTarget(t, mode string) (string, error) {
	t = strings.TrimSpace(t)
	port := "443"
	if mode == "http" {
		port = "80"
	}
	h, p, err := net.SplitHostPort(t)
	if err != nil {
		h = strings.Trim(t, "[]")
	} else if p != "" {
		port = p
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%s is not a port", port)
	}
	if h == "" || strings.ContainsAny(h, "/ @?#{}") || strings.Contains(h, "://") {
		return "", fmt.Errorf("give the target as host or host:port")
	}
	return net.JoinHostPort(h, port), nil
}

// ---- backup dests ----

var destNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ServerRef reads v as exactly one ${{ server.params.<collection>.<name> }}
// and nothing else (leaf/params owns the grammar): its collection.name key.
func ServerRef(v string) (key string, ok bool) {
	v = strings.TrimSpace(v)
	keys, err := params.ServerRefs(v)
	if err != nil || len(keys) != 1 || !strings.HasPrefix(v, "${{") || !strings.HasSuffix(v, "}}") || strings.Count(v, "${{") != 1 {
		return "", false
	}
	return keys[0], true
}

func (f *File) checkDests() error {
	seen := map[string]bool{}
	for i, d := range f.BackupDests {
		switch {
		case d.Name == "local":
			return fmt.Errorf("backup_dests: local is the install's own destination; a file does not declare it")
		case !destNameRe.MatchString(d.Name):
			return fmt.Errorf("backup_dests: %q: a name is letters, digits, - and _", d.Name)
		case seen[d.Name]:
			return fmt.Errorf("backup_dests: %s declared twice", d.Name)
		case d.Kind != "" && d.Kind != "s3":
			return fmt.Errorf("backup_dests.%s: kind is s3", d.Name)
		case strings.TrimSpace(d.Bucket) == "":
			return fmt.Errorf("backup_dests.%s: bucket required", d.Name)
		}
		seen[d.Name] = true
		if ep := strings.TrimRight(strings.TrimSpace(d.Endpoint), "/"); ep != "" {
			if u, err := url.Parse(ep); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("backup_dests.%s: endpoint is an http(s) URL, or empty for AWS", d.Name)
			}
			f.BackupDests[i].Endpoint = ep
		}
		f.BackupDests[i].Kind = "s3"
		f.BackupDests[i].Bucket = strings.TrimSpace(d.Bucket)
		f.BackupDests[i].Region = strings.TrimSpace(d.Region)
		for field, v := range map[string]string{"access_key": d.AccessKey, "secret_key": d.SecretKey} {
			if _, ok := ServerRef(v); !ok {
				return fmt.Errorf("backup_dests.%s: %s is a ${{ server.params.<collection>.<name> }} ref, never the value", d.Name, field)
			}
		}
		if _, ok := ServerRef(d.ArchiveKey); d.ArchiveKey != "" && !ok {
			return fmt.Errorf("backup_dests.%s: archive_key is a ${{ server.params.<collection>.<name> }} ref, never the value", d.Name)
		}
	}
	return nil
}

// ---- domains ----

func (f *File) checkDomains() error {
	seen, froms := map[string]bool{}, map[string]bool{}
	for i, d := range f.Domains {
		if strings.TrimSpace(d.Host) == "" {
			return fmt.Errorf("domains: host required")
		}
		host := installspec.CleanHost(d.Host)
		if seen[host] {
			return fmt.Errorf("domains: %s declared twice", host)
		}
		seen[host] = true
		f.Domains[i].Host = host
		if d.From != "" {
			from := installspec.CleanHost(d.From)
			if from == host {
				return fmt.Errorf("domains: %s renames to itself", host)
			}
			if froms[from] {
				return fmt.Errorf("domains: %s is renamed twice", from)
			}
			froms[from] = true
			f.Domains[i].From = from
		}
	}
	for _, d := range f.Domains {
		if d.From != "" && seen[d.From] {
			return fmt.Errorf("domains: %s is both renamed away and declared", d.From)
		}
	}
	return nil
}

// ---- connectors and orgs ----

func (f *File) checkConnectors() error {
	seen := map[string]bool{}
	for _, c := range f.Connectors {
		switch {
		case strings.TrimSpace(c.Name) == "":
			return fmt.Errorf("connectors: name required")
		case seen[c.Name]:
			return fmt.Errorf("connectors: %s declared twice", c.Name)
		}
		seen[c.Name] = true
		if c.Share == nil {
			continue
		}
		for _, o := range c.Share.Orgs {
			if !slug.Valid(o) || slug.Reserved(o) {
				return fmt.Errorf("connectors.%s: %q is not an org slug", c.Name, o)
			}
		}
	}
	return nil
}

func (f *File) checkOrgs() error {
	seen := map[string]bool{}
	for i, o := range f.Orgs {
		switch {
		case !slug.Valid(o.Slug) || slug.Reserved(o.Slug):
			return fmt.Errorf("orgs: %q is not an org slug", o.Slug)
		case seen[o.Slug]:
			return fmt.Errorf("orgs: %s declared twice", o.Slug)
		case o.Repo == "" && (o.Branch != "" || o.Path != "" || o.Connector != "" || o.Auto):
			return fmt.Errorf("orgs.%s: branch, path, connector and auto go with repo", o.Slug)
		}
		seen[o.Slug] = true
		f.Orgs[i].Repo = githubapp.RepoURL(o.Repo)
	}
	return nil
}
