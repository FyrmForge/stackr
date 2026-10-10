// Package orgconfig is the org config file (stackr-org.yml): Parse reads it,
// Diff compares it with a Live snapshot the orchestrator gathers. Pure: no
// store, no clone, no enqueue. REWRITE.md "Org config file".
package orgconfig

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
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
	Version   int                 `yaml:"version"`         // must be 1
	Org       string              `yaml:"org"`             // the name; a slug change is a rename
	Tiers     Tiers               `yaml:"tiers,omitempty"` // bottom first; none: params use the env keys all and pr
	Params    Params              `yaml:"params,omitempty"`
	Defaults  *Defaults           `yaml:"defaults,omitempty"`   // nil: the key is absent, say nothing
	EnvColors map[string]string   `yaml:"env_colors,omitempty"` // nil: the key is absent, say nothing
	Stacks    map[string]StackRef `yaml:"stacks,omitempty"`
	Domains   []Reservation       `yaml:"domains,omitempty"`
	Shares    map[string]Share    `yaml:"shares,omitempty"` // nil: the key is absent, say nothing; {} removes every share
	Moved     []Move              `yaml:"moved,omitempty"`
}

// Tier is one org tier: an env named like it joins it. A tier starts locked.
type Tier struct {
	Slug   string
	Locked bool
}

// Tiers is the tiers: map in file order, bottom first.
type Tiers []Tier

type tierBody struct {
	Locked *bool `yaml:"locked"`
}

func (ts *Tiers) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: tiers is a map of slug to {locked: bool}, bottom first", n.Line)
	}
	*ts = nil
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		t := Tier{Slug: k.Value, Locked: true}
		switch {
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null": // `dev:` alone
		case v.Kind != yaml.MappingNode:
			return fmt.Errorf("line %d: tiers.%s is {locked: bool}", v.Line, k.Value)
		default:
			var body tierBody
			if err := strictNode(v, &body); err != nil {
				return fmt.Errorf("tiers.%s: %w", k.Value, err)
			}
			if body.Locked != nil {
				t.Locked = *body.Locked
			}
		}
		*ts = append(*ts, t)
	}
	return nil
}

func (ts Tiers) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, t := range ts {
		var k, v yaml.Node
		if err := k.Encode(t.Slug); err != nil {
			return nil, err
		}
		if err := v.Encode(struct {
			Locked bool `yaml:"locked"`
		}{t.Locked}); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &k, &v)
	}
	return n, nil
}

// Slugs are the tier slugs, bottom first.
func (ts Tiers) Slugs() []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Slug
	}
	return out
}

// All is the env key of an org with no tiers: the org scope. pr is org_pr.
const All = "all"

// Params is the file's params:, one grammar for every org: env key -> name.
// The env keys are tier slugs and pr with tiers, all and pr without.
type Params struct {
	Tiered map[string]planfile.Group
	node   yaml.Node
}

func (p *Params) UnmarshalYAML(n *yaml.Node) error {
	p.node = *n
	return nil
}

func (p Params) MarshalYAML() (any, error) { return p.Tiered, nil }

func (p Params) IsZero() bool { return len(p.Tiered) == 0 && p.node.Kind == 0 }

// Blocks is the params as one block per env key (a tier, all, or pr): key ->
// "group.name" -> entry. Nil when the file has no params:.
func (f *File) Blocks() map[string]map[string]planfile.Entry {
	if f.Params.Tiered == nil {
		return nil
	}
	b, _ := planfile.Expand(f.Params.Tiered) // Parse checked it
	return b
}

// resolve decodes the params: node and checks its env keys against tiers:.
func (p *Params) resolve(tiers Tiers) error {
	if p.node.Kind == 0 {
		return nil
	}
	b, err := yaml.Marshal(&p.node)
	if err != nil {
		return err
	}
	if err := planfile.StrictYAML(b, &p.Tiered, nil); err != nil {
		return err
	}
	blocks, err := planfile.Expand(p.Tiered)
	if err != nil {
		return err
	}
	if len(tiers) == 0 {
		for _, env := range slices.Sorted(maps.Keys(blocks)) {
			if env != All && env != planfile.PR {
				return fmt.Errorf("params: %q is not an env key of an org with no tiers (all or pr)", env)
			}
		}
		return nil
	}
	if _, ok := blocks[All]; ok {
		return fmt.Errorf("params: all is for an org with no tiers; use a tier slug or pr")
	}
	return planfile.CheckEnvKeys(blocks, tiers.Slugs(), "a tier of this org")
}

// strictNode decodes a node under a custom unmarshaler, which does not
// inherit the decoder's KnownFields.
func strictNode(n *yaml.Node, out any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return planfile.StrictYAML(b, out, nil)
}

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
	From string `yaml:"from"` // stack.<slug> or tier.<slug>
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
	var err error
	if err = planfile.StrictYAML(data, &f, removedKeys); err != nil {
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
	if err := checkTiers(f.Tiers); err != nil {
		return nil, err
	}
	if err := f.Params.resolve(f.Tiers); err != nil {
		return nil, err
	}
	if err = planfile.CheckParams(f.Params.Tiered, true); err != nil {
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

func checkTiers(ts Tiers) error {
	seen := map[string]bool{}
	for _, t := range ts {
		switch {
		case !slug.Valid(t.Slug) || tier.Reserved(t.Slug):
			return fmt.Errorf("tiers: %q is not a tier slug (pr and order are reserved)", t.Slug)
		case seen[t.Slug]:
			return fmt.Errorf("tiers: %s declared twice", t.Slug)
		}
		seen[t.Slug] = true
	}
	return nil
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
		if (kind != "stack" && kind != "tier") || !slug.Valid(s) {
			return fmt.Errorf("moved: %q is stack.<slug> or tier.<slug>", ref)
		}
	}
	if k, _, _ := strings.Cut(m.From, "."); !strings.HasPrefix(m.To, k+".") {
		return fmt.Errorf("moved: %s and %s are not the same kind", m.From, m.To)
	}
	if m.From == m.To {
		return fmt.Errorf("moved: %s moves nowhere", m.From)
	}
	return nil
}
