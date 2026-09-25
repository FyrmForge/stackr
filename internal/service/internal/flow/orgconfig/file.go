// Package orgconfig is the org config file (stackr-org.yml): Parse reads it,
// Diff compares it with a Live snapshot the orchestrator gathers. Pure: no
// store, no clone, no enqueue. REWRITE.md "Org config file".
package orgconfig

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// stackFilePath is promote.DefaultPath, where a stack file lives when a
// binding names no path; flows do not import flows.
const stackFilePath = "stackr-compose.yml"

// File is the org file as written.
type File struct {
	Version   int                         `yaml:"version"` // must be 1
	Org       string                      `yaml:"org"`     // the name; a slug change is a rename
	Params    map[string]map[string]Param `yaml:"params"`
	Defaults  *Defaults                   `yaml:"defaults"`   // nil: the key is absent, say nothing
	EnvColors map[string]string           `yaml:"env_colors"` // nil: the key is absent, say nothing
	Stacks    map[string]StackRef         `yaml:"stacks"`
	Shared    map[string]SharedConf       `yaml:"shared"`
	Domains   []Reservation               `yaml:"domains"`
	Moved     []Move                      `yaml:"moved"`
}

// Param is one declaration, the stack file's grammar. A secret is name and
// type only: the file is in git.
type Param struct {
	Type  string  `yaml:"type"` // param | secret
	Value *string `yaml:"value"`
}

// Defaults is the org rung of the settings cascade, leaf/settings.Settings
// with yaml keys; nil = say nothing here.
type Defaults struct {
	CPULimit        *float64 `yaml:"cpu_limit" json:"cpu_limit,omitempty"`
	MemLimitMB      *int     `yaml:"mem_limit_mb" json:"mem_limit_mb,omitempty"`
	Protect         *bool    `yaml:"protect" json:"protect,omitempty"`
	ProtectUser     *string  `yaml:"protect_user" json:"protect_user,omitempty"`
	ProtectPassword *string  `yaml:"protect_password" json:"protect_password,omitempty"`
}

// StackRef says where a stack's file is and nothing else (DECIDE 182):
// repo (with branch, path, connector) for the stack's own repo, or path
// alone for a file inside the org repo.
type StackRef struct {
	Repo      string `yaml:"repo"`
	Branch    string `yaml:"branch"`
	Path      string `yaml:"path"`
	Connector string `yaml:"connector"` // a connector id; "" = the org's connector for the repo's host
}

// UnmarshalYAML refuses every key but the four: anything else is v0's
// inline stack, which the rewrite has no apply path for.
func (r *StackRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a stack entry is repo, branch, path and connector, or path alone", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch k := n.Content[i]; k.Value {
		case "repo", "branch", "path", "connector":
		default:
			return fmt.Errorf("line %d: %s: inline stacks are not supported; put the stack in its own file", k.Line, k.Value)
		}
	}
	type plain StackRef
	return n.Decode((*plain)(r))
}

// Binding is the four config repo columns a StackRef stands for.
type Binding struct {
	ConnectorID string
	Repo        string
	Branch      string
	Path        string
}

// Binding is what the stack's config repo columns should hold. A path-alone
// entry is a file in the org repo, so it takes the org's binding.
func (r StackRef) Binding(org store.Org) Binding {
	if r.Repo == "" {
		return Binding{
			ConnectorID: org.ConfigConnectorID,
			Repo:        org.ConfigRepo,
			Branch:      org.ConfigBranch,
			Path:        r.Path,
		}
	}
	return Binding{
		ConnectorID: r.Connector,
		Repo:        githubapp.RepoURL(r.Repo),
		Branch:      r.Branch,
		Path:        r.Path,
	}
}

// SharedConf is one org-scoped managed instance (DECIDE 183).
type SharedConf struct {
	Engine    string `yaml:"engine"`      // postgres | s3
	Host      string `yaml:"host"`        // <stack>/<env>, the env whose container runs it
	Image     string `yaml:"image"`       // "" = the engine's default, never compared
	ShmSizeMB *int   `yaml:"shm_size_mb"` // nil = never compared
}

// Reservation is one org domain resource (DECIDE 191).
type Reservation struct {
	Host                string `yaml:"host"`
	ACMEEmail           string `yaml:"acme_email"`
	IncludeEnvOnDefault bool   `yaml:"include_env_on_default"`
}

// Move is one rename, read before the rest of the file (DECIDE 184).
type Move struct {
	From string `yaml:"from"` // stack.<slug> | shared.<slug>
	To   string `yaml:"to"`
}

// removedKeys tells a v0 org file what replaced a key.
var removedKeys = map[string]string{
	"vars":     "declare them under params:",
	"secrets":  "declare them under params: with type: secret",
	"storage":  "storage shares are not in v1",
	"ui_edits": "drop it; drift never promotes",
}

var unknownField = regexp.MustCompile(`^(?:line \d+: )?field (\S+) not found in type \S+$`)

// Parse decodes and checks the whole file; Diff trusts what it returns.
func Parse(data []byte) (*File, error) {
	var f File
	if err := strictYAML(data, &f); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d (want 1)", f.Version)
	}
	if strings.TrimSpace(f.Org) == "" {
		return nil, fmt.Errorf("org: required, the organization's name")
	}
	if slug.Make(f.Org) == "" {
		return nil, fmt.Errorf("org: %q needs at least one letter or digit", f.Org)
	}
	if err := checkParams(f.Params); err != nil {
		return nil, err
	}
	if f.Defaults != nil && (f.Defaults.ProtectUser == nil) != (f.Defaults.ProtectPassword == nil) {
		return nil, fmt.Errorf("defaults: protect_user and protect_password go together")
	}
	for env := range f.EnvColors {
		if !slug.Valid(env) {
			return nil, fmt.Errorf("env_colors: %q is not an env slug", env)
		}
	}
	for s, r := range f.Stacks {
		if err := checkStack(s, r); err != nil {
			return nil, err
		}
	}
	for s, c := range f.Shared {
		if err := checkShared(s, c); err != nil {
			return nil, err
		}
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
	for _, m := range f.Moved {
		if err := checkMove(m); err != nil {
			return nil, err
		}
	}
	return &f, nil
}

func checkStack(s string, r StackRef) error {
	switch {
	case !slug.Valid(s) || slug.Reserved(s):
		return fmt.Errorf("stacks: %q is not a stack slug", s)
	case r.Repo == "" && r.Path == "":
		return fmt.Errorf("stacks.%s: give repo, or path for a file in the org repo", s)
	case r.Repo == "" && (r.Branch != "" || r.Connector != ""):
		return fmt.Errorf("stacks.%s: branch and connector go with repo; path alone is a file in the org repo", s)
	}
	return nil
}

func checkShared(s string, c SharedConf) error {
	st, env, _ := strings.Cut(c.Host, "/")
	switch {
	case !slug.Valid(s) || slug.Reserved(s):
		return fmt.Errorf("shared: %q is not a tile slug", s)
	case c.Engine == "":
		return fmt.Errorf("shared.%s: engine required", s)
	case c.Host == "":
		return fmt.Errorf("shared.%s: host required, the <stack>/<env> that runs it", s)
	case !slug.Valid(st) || !slug.Valid(env):
		return fmt.Errorf("shared.%s: host %q is <stack>/<env>", s, c.Host)
	case c.ShmSizeMB != nil && *c.ShmSizeMB < 0:
		return fmt.Errorf("shared.%s: shm_size_mb is not negative", s)
	}
	// ponytail: an unknown engine is refused at apply (CreateManagedTile);
	// the engine list lives in flow/managed, which this flow may not import.
	return nil
}

// moveKinds maps a moved: prefix to its rename change.
var moveKinds = map[string]string{
	"stack":  "rename",
	"shared": "instance-rename",
}

func checkMove(m Move) error {
	for _, ref := range []string{m.From, m.To} {
		kind, s, _ := strings.Cut(ref, ".")
		if moveKinds[kind] == "" || !slug.Valid(s) {
			return fmt.Errorf("moved: %q is stack.<slug> or shared.<slug>", ref)
		}
	}
	fk, fs, _ := strings.Cut(m.From, ".")
	tk, ts, _ := strings.Cut(m.To, ".")
	switch {
	case fk != tk:
		return fmt.Errorf("moved: %s to %s: from and to are the same kind", m.From, m.To)
	case fs == ts:
		return fmt.Errorf("moved: %s moves nowhere", m.From)
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

// strictYAML decodes refusing unknown keys, top level included.
// ponytail: strictYAML, humanYAML and checkParams are copies of
// flow/promote's (flows do not import flows); a third copy moves them into
// a shared package.
func strictYAML(data []byte, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && err != io.EOF {
		return humanYAML(err)
	}
	return nil
}

// humanYAML rewrites yaml.v3's unknown-key errors, which name a Go type.
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
