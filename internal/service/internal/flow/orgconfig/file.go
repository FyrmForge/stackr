// Package orgconfig is the org config file (stackr-org.yml): Parse reads it,
// Diff compares it with a Live snapshot the orchestrator gathers. Pure: no
// store, no clone, no enqueue. REWRITE.md "Org config file".
package orgconfig

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// stackFilePath is promote.DefaultPath, where a stack file lives when a
// binding names no path; flows do not import flows.
const stackFilePath = "stackr-compose.yml"

// DefaultPath is where the org file lives when the binding names no path.
const DefaultPath = "stackr-org.yml"

// File is the org file as written. The omitempty tags are for export: an
// empty map or a nil pointer would print as flow style or null.
type File struct {
	Version   int                         `yaml:"version"` // must be 1
	Org       string                      `yaml:"org"`     // the name; a slug change is a rename
	Params    map[string]map[string]Param `yaml:"params,omitempty"`
	Defaults  *Defaults                   `yaml:"defaults,omitempty"`   // nil: the key is absent, say nothing
	EnvColors map[string]string           `yaml:"env_colors,omitempty"` // nil: the key is absent, say nothing
	Stacks    map[string]StackRef         `yaml:"stacks,omitempty"`
	Domains   []Reservation               `yaml:"domains,omitempty"`
	Shares    map[string]Share            `yaml:"shares,omitempty"` // nil: the key is absent, say nothing; {} removes every share
	Moved     []Move                      `yaml:"moved,omitempty"`
}

// Param is one declaration, the stack file's grammar.
type Param = planfile.Param

// Defaults is the org rung of the settings cascade, leaf/settings.Settings
// with yaml keys; nil = say nothing here.
type Defaults struct {
	CPULimit        *float64 `yaml:"cpu_limit,omitempty" json:"cpu_limit,omitempty"`
	MemLimitMB      *int     `yaml:"mem_limit_mb,omitempty" json:"mem_limit_mb,omitempty"`
	Protect         *bool    `yaml:"protect,omitempty" json:"protect,omitempty"`
	ProtectUser     *string  `yaml:"protect_user,omitempty" json:"protect_user,omitempty"`
	ProtectPassword *string  `yaml:"protect_password,omitempty" json:"protect_password,omitempty"`
}

// StackRef says where a stack's file is and nothing else (DECIDE 182):
// repo (with branch, path, connector) for the stack's own repo, or path
// alone for a file inside the org repo.
type StackRef struct {
	Repo      string `yaml:"repo,omitempty"`
	Branch    string `yaml:"branch,omitempty"`
	Path      string `yaml:"path,omitempty"`
	Connector string `yaml:"connector,omitempty"` // a connector id; "" = the org's connector for the repo's host
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

// Reservation is one org domain resource (DECIDE 191).
type Reservation struct {
	Host                string `yaml:"host"`
	ACMEEmail           string `yaml:"acme_email,omitempty"`
	IncludeEnvOnDefault bool   `yaml:"include_env_on_default"`
}

// Share is one network share (leaf/volume). User and Password are plain text
// and an ${{ org.params }} ref: the file is in git, so never the password.
type Share struct {
	Kind     string `yaml:"kind"` // nfs | smb
	Source   string `yaml:"source"`
	Options  string `yaml:"options,omitempty"`
	User     string `yaml:"user,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// Spec is the share as the volume leaf takes it.
func (s Share) Spec(slug string) volume.ShareSpec {
	return volume.ShareSpec{
		Slug:        slug,
		Kind:        s.Kind,
		Source:      s.Source,
		Options:     s.Options,
		User:        s.User,
		PasswordRef: s.Password,
	}
}

// Move is one rename, read before the rest of the file (DECIDE 184).
type Move struct {
	From string `yaml:"from"` // stack.<slug>
	To   string `yaml:"to"`
}

// removedKeys tells a v0 org file what replaced a key.
var removedKeys = map[string]string{
	"vars":     "declare them under params:",
	"secrets":  "declare them under params: with type: secret",
	"storage":  "network shares are under shares:",
	"ui_edits": "drop it; drift never promotes",
}

// Parse decodes and checks the whole file; Diff trusts what it returns.
func Parse(data []byte) (*File, error) {
	var f File
	if err := planfile.StrictYAML(data, &f, removedKeys); err != nil {
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
	if err := planfile.CheckParams(f.Params); err != nil {
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
	for sl, sh := range f.Shares {
		if err := volume.CheckShare(sh.Spec(sl)); err != nil {
			return nil, fmt.Errorf("shares.%s: %w", sl, err)
		}
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

func checkMove(m Move) error {
	for _, ref := range []string{m.From, m.To} {
		kind, s, _ := strings.Cut(ref, ".")
		if kind != "stack" || !slug.Valid(s) {
			return fmt.Errorf("moved: %q is stack.<slug>", ref)
		}
	}
	if m.From == m.To {
		return fmt.Errorf("moved: %s moves nowhere", m.From)
	}
	return nil
}
