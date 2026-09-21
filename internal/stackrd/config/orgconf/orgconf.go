// Package orgconf is org-level config-as-code (§6): one file from which a
// fresh stackr server can materialize the entire org, org vars/secrets,
// org-scoped shared managed instances, and the stacks themselves. It leans on
// stackconf for every shape it can (tile decoding, plan/change rendering,
// the FileSource fetcher) so there is exactly one config grammar.
//
// Org plans are ALWAYS manual: an org apply can create and delete whole
// stacks, and no webhook is allowed to do that unattended. That is the §6
// delete-protection for v1, a human approves every org plan.
package orgconf

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	yaml "go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// DefaultPath is where the org file lives unless the binding says otherwise.
const DefaultPath = "stackr-org.yml"

// ErrNotBound / ErrNoFile mirror stackconf's runner sentinels.
var (
	ErrNotBound = fmt.Errorf("org is not bound to a config repo")
	ErrNoFile   = fmt.Errorf("no org config file on this branch")
)

// File is the parsed org config.
type File struct {
	Version int                   `yaml:"version"`
	Org     string                `yaml:"org"`
	Vars    map[string]string     `yaml:"vars,omitempty"`
	Secrets stackconf.SecretsNode `yaml:"secrets,omitempty"`
	// Shared: org-scoped managed instances, same tile grammar as stackconf's
	// shared: block, scope is structural (this block = org).
	Shared map[string]stackconf.RawMap `yaml:"shared,omitempty"`
	Stacks map[string]StackRef         `yaml:"stacks,omitempty"`
	// Moved declares renames of keyed things in this file: stack.X and
	// shared.X. Same grammar as the stack file's (stackconf/moved.go), scoped
	// to this org because the file's own scope is the namespace.
	Moved []stackconf.MovedEntry `yaml:"moved,omitempty"`
	// Defaults are org-level settings every declared stack inherits unless its
	// own file overrides them, the first org -> stack fallback in the format.
	Defaults Defaults `yaml:"defaults,omitempty"`
	// Domains are the org's domain resources: the bases every stack in the org
	// generates hostnames under unless it declares its own.
	Domains []stackconf.DomainResConf `yaml:"domains,omitempty"`
	// Storage is the org's network shares (storage.go).
	Storage map[string]StorageConf `yaml:"storage,omitempty"`
}

// Defaults is the defaults: block.
type Defaults struct {
	// UIEdits: block | stage. See repo.Stack.UIEdits.
	UIEdits string `yaml:"ui_edits,omitempty"`
	// EnvColors: env slug to colour (palette name or #rrggbb), the default
	// for every environment with that slug in this org. See envcolor.
	EnvColors map[string]string `yaml:"env_colors,omitempty"`
	// The defaults cascade's org level (internal/stackrd/config/settings).
	// The same keys the stack file's own defaults: takes, one rung up.
	stackconf.DefaultsConf `yaml:",inline"`
}

// StackRef is one declared stack, in one of three source forms:
//   - repo:  external git repo (own stack file; optional path/branch/connector)
//   - path:  a stack file at a relative path in the ORG repo (monorepo form)
//   - inline: the whole stack file body, either under an inline: key or
//     flattened right under the stack's name (any stack-file top-level key
//     marks a body; version: and stack: are optional there, inherited from
//     the org file and the entry's key)
type StackRef struct {
	Repo      string         `yaml:"repo,omitempty"`
	Connector string         `yaml:"connector,omitempty"`
	Branch    string         `yaml:"branch,omitempty"`
	Path      string         `yaml:"path,omitempty"`
	Inline    map[string]any `yaml:"inline,omitempty"`
	// Protected ratchets the delete side: removing this stack from the file
	// is refused outright instead of planned.
	Protected bool `yaml:"protected,omitempty"`
}

// bodyKeys are stack-file top-level keys: any one of them marks a stacks:
// entry as a flattened inline body, since no reference form carries them.
// version: and stack: are optional in a body, inherited from the org file
// and the entry's key, but still recognized so files carrying them parse.
var bodyKeys = map[string]bool{"version": true, "stack": true, "base": true,
	"environments": true, "shared": true, "secrets": true, "pr_envs": true, "include": true, "proxy": true}

func (r *StackRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("stack entry must be a map")
	}
	// Flattened inline form: the stack body sits right under the name.
	// protected: is the org-level adornment shared with the reference form,
	// so it is peeled before the body meets the stack-file grammar (which
	// has no such key and would reject it).
	for i := 0; i < len(n.Content); i += 2 {
		if !bodyKeys[n.Content[i].Value] {
			continue
		}
		var body map[string]any
		if err := n.Decode(&body); err != nil {
			return err
		}
		if p, ok := body["protected"]; ok {
			b, isBool := p.(bool)
			if !isBool {
				return fmt.Errorf("protected must be true or false")
			}
			r.Protected = b
			delete(body, "protected")
		}
		r.Inline = body
		return nil
	}
	// Reference (or wrapped inline:) form, strict, because a custom
	// unmarshaler does not inherit the parent decoder's KnownFields.
	type plain StackRef // shed the method so decoding doesn't recurse
	var p plain
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return stackconf.HumanYAML(err)
	}
	*r = StackRef(p)
	return nil
}

func (r StackRef) form() string {
	switch {
	case len(r.Inline) > 0:
		return "inline"
	case r.Repo != "":
		return "repo"
	case r.Path != "":
		return "path"
	}
	return ""
}

// Parse decodes and validates the org file.
func Parse(data []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, stackconf.HumanYAML(err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("unsupported version %d (want 1)", f.Version)
	}
	if f.Org == "" {
		return nil, fmt.Errorf("org name required")
	}
	for slug, v := range f.Defaults.EnvColors {
		if !envcolor.Valid(v) {
			return nil, fmt.Errorf("defaults.env_colors.%s: %q is not a palette name or #rrggbb", slug, v)
		}
	}
	if err := stackconf.ValidateUIEdits(f.Defaults.UIEdits); err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	if err := stackconf.ValidateDomains(f.Domains); err != nil {
		return nil, err
	}
	if err := f.Defaults.Check(); err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	if err := validateStorage(f.Storage); err != nil {
		return nil, err
	}
	for name, raw := range f.Shared {
		tc, err := stackconf.DecodeTile(raw)
		if err != nil {
			return nil, fmt.Errorf("shared tile %s: %w", name, err)
		}
		if tc.Type != "managed" {
			return nil, fmt.Errorf("shared tile %s: only managed instances can be org-shared", name)
		}
		tc.Scope = "org"
		if err := stackconf.ValidateTileConf(name, tc); err != nil {
			return nil, fmt.Errorf("shared: %w", err)
		}
	}
	for name, ref := range f.Stacks {
		switch ref.form() {
		case "":
			return nil, fmt.Errorf("stack %s: needs one of repo:, path: or inline: keys", name)
		case "inline":
			if ref.Repo != "" || ref.Path != "" {
				return nil, fmt.Errorf("stack %s: inline excludes the repo:/path: keys", name)
			}
			// An inline body inherits the org file's schema version, two
			// version numbers in one file could only agree or be a bug.
			if _, ok := ref.Inline["version"]; !ok {
				ref.Inline["version"] = f.Version
			}
			// The org key names an inline stack (the instantiator names the
			// instance): the body's stack: defaults from the key and may not
			// disagree, a second name would just be the rename fight again.
			if s, ok := ref.Inline["stack"]; ok {
				str, _ := s.(string)
				if repo.Slugify(str) != repo.Slugify(name) {
					return nil, fmt.Errorf("stack %s: the inline body says stack: %v; the org key names it; drop stack: or make them match", name, s)
				}
			} else {
				ref.Inline["stack"] = name
			}
			// Validate the embedded stack file now, a broken inline stack
			// must fail the org plan, not the apply.
			b, err := yaml.Marshal(ref.Inline)
			if err != nil {
				return nil, fmt.Errorf("stack %s: %w", name, err)
			}
			if _, err := stackconf.Load(b, func(string) ([]byte, error) {
				return nil, fmt.Errorf("inline stacks cannot use the include: key")
			}); err != nil {
				return nil, fmt.Errorf("stack %s (inline): %w", name, err)
			}
		}
	}
	return &f, nil
}

// sharedConf decodes one shared entry with org scope applied.
func (f *File) sharedConf(name string) (stackconf.TileConf, error) {
	tc, err := stackconf.DecodeTile(f.Shared[name])
	tc.Scope = "org"
	return tc, err
}

// Runner plans and applies org config. Stacks/Applier are the stack-level
// machinery the org layer delegates to.
type Runner struct {
	Store   repo.Store
	Src     stackconf.FileSource
	Stacks  stackconf.Planner
	Applier stackconf.Applier
	// Resources owns the ACME account a domain resource's certificates are
	// issued on, and the traefik restart that changing it needs. Nil-checked:
	// tests that only diff do not wire it.
	Resources *service.DomainResourceService
	// StackSvc owns the stack row. An org file that declares a stack creates
	// and binds it through here, so it gets the slug rules, the production
	// environment and the staged-row drop that the panel's own create and
	// bind have always had. Required for Apply; a Runner that only diffs does
	// not reach it.
	StackSvc *service.StackService
}

// Load fetches and parses the org's bound file, returning the head sha too.
func (r Runner) Load(ctx context.Context, org *repo.Org) (*File, string, error) {
	if !org.ConfigManaged() {
		return nil, "", ErrNotBound
	}
	cn, err := r.Store.GetConnector(ctx, org.ConfigConnectorID)
	if err != nil || cn == nil {
		return nil, "", fmt.Errorf("org config connector: not found")
	}
	branch := org.ConfigBranch
	if branch == "" {
		if branch, err = r.Src.DefaultBranch(ctx, cn, org.ConfigRepo); err != nil {
			return nil, "", err
		}
	}
	sha, _ := r.Src.HeadSHA(ctx, cn, org.ConfigRepo, branch)
	path := org.ConfigPath
	if path == "" {
		path = DefaultPath
	}
	data, err := r.Src.FileContents(ctx, cn, org.ConfigRepo, branch, path)
	if err != nil {
		return nil, "", err
	}
	if data == nil {
		return nil, "", ErrNoFile
	}
	f, err := Parse(data)
	return f, sha, err
}

// orgTiles lists the org-scoped managed instances (the rows the org file owns
// exclusively, stack snapshots deliberately skip them).
func (r Runner) orgTiles(ctx context.Context, org *repo.Org) ([]repo.Tile, error) {
	stacks, err := r.Store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	var out []repo.Tile
	for _, s := range stacks {
		ts, err := r.Store.ListTilesByStack(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			if t.ScopeKind == "org" && t.ScopeID == org.ID && t.IsManaged() {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// Plan diffs the org file against the org's current state and stores the
// result. Status "clean" when nothing differs.
func (r Runner) Plan(ctx context.Context, org *repo.Org, sha string) (*repo.ConfigPlan, error) {
	f, headSHA, err := r.Load(ctx, org)
	if err == ErrNotBound || err == ErrNoFile {
		return nil, err
	}
	if sha == "" {
		sha = headSHA
	}
	if err != nil {
		return r.save(ctx, org, sha, &repo.ConfigPlan{Status: "error", Error: err.Error(), Summary: "org config invalid"})
	}
	r.markDeclared(ctx, org, f)
	p, err := r.diff(ctx, org, f)
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	status := "pending"
	if p.Empty() && len(p.Errors) == 0 {
		status = "clean"
	}
	return r.save(ctx, org, sha, &repo.ConfigPlan{Status: status, Summary: p.Summary(), Plan: string(blob)})
}

// PreviewBundle computes a plan from posted file bytes without storing it,
// the org twin of stackconf's PreviewBundle. Org files have no include:, so
// the bundle is one file. No markDeclared: preview must write nothing, and
// that refresh mutates stack rows.
func (r Runner) PreviewBundle(ctx context.Context, org *repo.Org, main []byte) (*stackconf.Plan, error) {
	if !org.ConfigManaged() {
		return nil, ErrNotBound
	}
	f, err := Parse(main)
	if err != nil {
		return nil, err
	}
	return r.diff(ctx, org, f)
}

func (r Runner) save(ctx context.Context, org *repo.Org, sha string, p *repo.ConfigPlan) (*repo.ConfigPlan, error) {
	if err := r.Store.SupersedePendingOrgPlans(ctx, org.ID); err != nil {
		return nil, err
	}
	p.ID = uuid.New().String()
	p.StackID = org.ID // org_config_plans: stack_id holds the org id
	p.CommitSHA = sha
	p.CreatedAt = time.Now().UTC()
	if err := r.Store.CreateOrgConfigPlan(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// declaredSlugs is the file's stacks: keys as slugs, the form stack rows are
// matched by everywhere (keys are display names, rows hold slugs).
func declaredSlugs(f *File) map[string]bool {
	out := make(map[string]bool, len(f.Stacks))
	for name := range f.Stacks {
		out[repo.Slugify(name)] = true
	}
	return out
}

// markDeclared refreshes each stack row's OrgDeclared flag to whether the
// file currently declares it, the fact stack plans gate stack-file renames
// on (the org file owns a declared stack's name). Done at plan time, not just
// apply: webhook pushes re-plan the org, so flags track the file without an
// apply ever running. Best-effort, a failed write leaves a stale flag, and
// the gate double-checks the org is still config-managed anyway.
func (r Runner) markDeclared(ctx context.Context, org *repo.Org, f *File) {
	stacks, err := r.Store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return
	}
	declared := declaredSlugs(f)
	for i := range stacks {
		s := &stacks[i]
		if s.OrgDeclared == declared[s.Slug] {
			continue
		}
		s.OrgDeclared = declared[s.Slug]
		if err := r.Store.UpdateStack(ctx, s); err != nil {
			slog.Error("stack org-declared flag not saved", "stack", s.ID, "declared", s.OrgDeclared, "error", err)
		}
	}
}

// diff computes the org plan. Change.Env carries "org" for org-level rows and
// the stack slug for stack lifecycle rows, which renders fine in the existing
// plan page.
func (r Runner) diff(ctx context.Context, org *repo.Org, f *File) (*stackconf.Plan, error) {
	p := &stackconf.Plan{}
	// Every row carries a note: the plan page hides it behind the row, and a
	// reviewer who opens one should learn what the apply does, not just what
	// the value becomes.
	upd := func(env, tile, field, old, new_, note string) {
		if old != new_ {
			p.Changes = append(p.Changes, stackconf.Change{Kind: "update", Env: env, Tile: tile, Field: field, Old: old, New: new_, Note: note})
		}
	}

	// Renames first: without them a moved key is a delete and a create.
	r.planMoves(ctx, org, f, p)

	// Org identity rides the binding (connector+repo+path), so org: is desired
	// state like everything else, a mismatch plans a rename. A name that
	// slugifies to nothing can't be renamed to.
	if want := repo.Slugify(f.Org); want != "" && want != org.Slug {
		other, _ := r.Store.GetOrgBySlug(ctx, want)
		// The registry namespace is the org slug and a docker registry has no
		// rename: moving it would orphan every image, or mean re-tagging each
		// one (a blob mount plus a manifest push per tag).
		hasImages, _ := registry.OrgHasImages(ctx, r.Store, org.ID)
		switch {
		case hasImages:
			p.Errors = append(p.Errors, "org rename to "+want+
				" is refused: this organization has images in the registry under \""+org.Slug+
				"\", and renaming would orphan them")
		case other != nil && other.ID != org.ID:
			p.Errors = append(p.Errors, "org rename to "+want+" collides with an existing org")
		default:
			p.Changes = append(p.Changes, stackconf.Change{Kind: "update", Env: "org", Field: "slug",
				Old: org.Slug, New: want, Note: "renames the org. Its URLs and CLI paths change, its domains stay as they are"})
		}
	}

	// Org domain resources: the same shape as a stack's domains:, owned here.
	// A host another owner already holds is a plan error, the host index is
	// unique server-wide, so the insert would fail mid-apply.
	//
	// Only rows the file created (Declared) are the file's to delete, the same
	// rule this file already applies to clickops stacks below. Naming a panel
	// row in domains: adopts it; the plan says so.
	allRes, err := r.Store.ListDomainResources(ctx)
	if err != nil {
		return nil, err
	}
	ownRes := map[string]repo.DomainResource{}
	for _, res := range allRes {
		if res.Level == "org" && res.OwnerID == org.ID {
			ownRes[res.Host] = res
		}
	}
	wantRes := map[string]bool{}
	for _, d := range f.Domains {
		wantRes[d.Host] = true
		cur, ok := ownRes[d.Host]
		if !ok {
			for _, other := range allRes {
				if other.Host == d.Host {
					p.Errors = append(p.Errors, "domains: "+d.Host+" is already claimed on this server")
				}
			}
			p.Changes = append(p.Changes, stackconf.Change{Kind: "create", Env: "org", Field: "domain", New: d.Host,
				Note: "every stack in the org can generate hostnames under it"})
			continue
		}
		upd("org", "domain", d.Host, "include_env_on_default="+boolStr(cur.IncludeEnvOnDefault),
			"include_env_on_default="+boolStr(d.IncludeEnvOnDefault), "changes generated hostnames on the default environment")
		upd("org", "domain", d.Host, "acme_email="+defStr(cur.ACMEEmail, "(instance default)"), "acme_email="+defStr(d.ACMEEmail, "(instance default)"),
			"certificates under this host move to another Let's Encrypt account; traefik restarts once")
		if !cur.Declared && cur.IncludeEnvOnDefault == d.IncludeEnvOnDefault {
			p.Changes = append(p.Changes, stackconf.Change{Kind: "update", Env: "org", Field: "domain", New: d.Host,
				Note: "added in the panel; the file adopts it, and dropping it from the file will delete it"})
		}
	}
	for _, host := range sortedKeys(ownRes) {
		if !wantRes[host] && ownRes[host].Declared {
			p.Changes = append(p.Changes, stackconf.Change{Kind: "delete", Env: "org", Field: "domain", Old: host,
				Note: "hostnames already generated under it keep working until their tile redeploys"})
		}
	}

	if err := r.diffStorage(ctx, org, f, p); err != nil {
		return nil, err
	}

	// Org vars: additive + update. Removal is manual, the org may carry
	// clickops vars the file never declared, and nothing marks origin.
	cur, err := r.Store.ListVariables(ctx, repo.OwnerOrg, org.ID)
	if err != nil {
		return nil, err
	}
	curVals := map[string]string{}
	curSecret := map[string]bool{}
	for _, v := range cur {
		curVals[v.Name] = v.Value
		curSecret[v.Name] = v.Secret
	}
	for _, name := range sortedKeys(f.Vars) {
		if curSecret[name] {
			p.Errors = append(p.Errors, "var "+name+" is a secret on the org; declare it under secrets:, not vars:")
			continue
		}
		upd("org", "vars", name, curVals[name], f.Vars[name], "sets an org variable; any stack reads it as ${{ org.vars."+name+" }}")
	}
	for _, name := range sortedKeys(f.Secrets) {
		sc := f.Secrets[name]
		if _, set := curVals[name]; set {
			continue
		}
		switch sc.Default {
		case "generated":
			p.Changes = append(p.Changes, stackconf.GenChange("org", name))
		default:
			// Required says the org needs the value, not that the org may not
			// be created without it. The row collects it instead.
			p.Changes = append(p.Changes, stackconf.InputChange(stackconf.Input{Scope: "org", Name: name, Secret: true, Required: sc.Required}))
			p.Inputs = append(p.Inputs, stackconf.Input{Scope: "org", Name: name, Secret: true, Required: sc.Required})
		}
	}

	// Shared org-scoped instances.
	curTiles, err := r.orgTiles(ctx, org)
	if err != nil {
		return nil, err
	}
	bySlug := map[string]*repo.Tile{}
	for i := range curTiles {
		bySlug[curTiles[i].Slug] = &curTiles[i]
	}
	for _, name := range sortedKeys(f.Shared) {
		tc, err := f.sharedConf(name)
		if err != nil {
			return nil, err
		}
		t, ok := bySlug[name]
		if !ok {
			p.Changes = append(p.Changes, stackconf.Change{Kind: "create", Env: "org", Tile: name, New: "managed",
				Note: "creates an org-scoped " + tc.Engine + " instance; stacks take slices of it"})
			continue
		}
		if t.Engine != tc.Engine {
			note := "engine " + t.Engine + " → " + tc.Engine + " replaces the instance; its data volume is left behind"
			p.Changes = append(p.Changes,
				stackconf.Change{Kind: "delete", Env: "org", Tile: name, Note: note},
				stackconf.Change{Kind: "create", Env: "org", Tile: name, New: "managed", Note: note})
			continue
		}
		def := managedtiles.Engines[t.Engine].DefaultImage
		upd("org", name, "image", defStr(t.ImageRef, def), defStr(tc.Image, def), "changes the instance image; it restarts on the new one")
		upd("org", name, "shm_size_mb", fmt.Sprint(t.ShmSizeMB), fmt.Sprint(tc.ShmSizeMB), "changes the instance shm size; it restarts with the new value")
		upd("org", name, "external_port", fmt.Sprint(t.ExternalPort), fmt.Sprint(tc.ExternalPort), "changes the instance external port; it restarts with the new value")
	}
	for slug := range bySlug {
		if _, declared := f.Shared[slug]; !declared {
			p.Changes = append(p.Changes, stackconf.Change{Kind: "delete", Env: "org", Tile: slug,
				Note: "org-scoped instance not in the file; its slices and consumers lose it"})
		}
	}

	// Stacks. Deletion only reaches config-managed stacks: a clickops stack
	// the file never declared is not the file's to delete.
	stacks, err := r.Store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	stackBySlug := map[string]*repo.Stack{}
	for i := range stacks {
		stackBySlug[stacks[i].Slug] = &stacks[i]
	}
	for _, name := range sortedKeys(f.Stacks) {
		ref := f.Stacks[name]
		s, ok := stackBySlug[repo.Slugify(name)]
		if !ok {
			note := "creates the stack from the body in this file; its tiles are planned on the stack's own page after apply"
			if ref.form() != "inline" {
				wantRepo, _, _, wantPath := r.refBinding(org, ref)
				note = "creates the stack bound to " + wantRepo + "/" + wantPath + "; its own config is planned after apply"
			}
			p.Changes = append(p.Changes, stackconf.Change{Kind: "create", Env: name, New: "stack (" + ref.form() + ")", Note: note})
			continue
		}
		if ref.form() == "inline" {
			// Inline stacks re-apply their body every org apply; the diff of
			// their contents is the stack plan's business.
			continue
		}
		wantRepo, wantConn, wantBranch, wantPath := r.refBinding(org, ref)
		rebind := "the stack re-plans from its new binding"
		upd(name, "config", "repo", s.ConfigRepo, wantRepo, rebind)
		upd(name, "config", "connector", s.ConfigConnectorID, wantConn, rebind)
		upd(name, "config", "branch", s.ConfigBranch, wantBranch, rebind)
		upd(name, "config", "path", s.ConfigPath, wantPath, rebind)
	}
	// Membership by slugified key: keys are display names, rows hold slugs,
	// a raw-key lookup would read every non-slug-shaped key as a deletion.
	declared := declaredSlugs(f)
	for slug, s := range stackBySlug {
		if declared[slug] {
			continue
		}
		if !s.ConfigManaged() {
			continue // clickops stack, not the file's to delete
		}
		p.Changes = append(p.Changes, stackconf.Change{Kind: "delete", Env: slug,
			Note: "config-managed stack not in the org file; applying deletes the stack and everything in it"})
	}
	p.InputsFirst()
	return p, nil
}

// refBinding resolves a stack ref's binding: the path form inherits the org's
// own repo/connector/branch.
func (r Runner) refBinding(org *repo.Org, ref StackRef) (repoFull, connector, branch, path string) {
	if ref.form() == "repo" {
		conn := ref.Connector
		if conn == "" {
			conn = org.ConfigConnectorID
		}
		return ref.Repo, conn, ref.Branch, ref.Path
	}
	return org.ConfigRepo, org.ConfigConnectorID, defStr(ref.Branch, org.ConfigBranch), ref.Path
}

// Apply executes a stored org plan: re-fetches at the reviewed sha's branch,
// applies vars/secrets/instances/stacks, then kicks a stack replan for every
// bound stack. Always human-approved, there is no auto path.
func (r Runner) Apply(ctx context.Context, org *repo.Org, cp *repo.ConfigPlan) error {
	f, _, err := r.Load(ctx, org)
	if err != nil {
		_ = r.Store.SetOrgConfigPlanError(ctx, cp.ID, err.Error())
		return err
	}
	p, err := r.diff(ctx, org, f)
	if err != nil {
		return err
	}
	if len(p.Errors) > 0 {
		return fmt.Errorf("plan has errors: %s", strings.Join(p.Errors, "; "))
	}
	now := time.Now().UTC()

	// Declared renames before anything reads a slug. The row keeps its id, so
	// nothing hanging off it is destroyed.
	if err := r.applyMoves(ctx, org, f); err != nil {
		return err
	}

	// Rename first so the org pointer carries the new slug for everything
	// downstream (callers redirect off it too). The collision case is a plan
	// error, so it never reaches here.
	changed := false
	if want := repo.Slugify(f.Org); want != "" && want != org.Slug {
		// Gated here too, not only in the plan: an apply that skipped its plan
		// (a webhook push plans and applies in one go) would otherwise orphan
		// every image under the old namespace.
		has, herr := registry.OrgHasImages(ctx, r.Store, org.ID)
		if herr != nil {
			return herr
		}
		if has {
			return fmt.Errorf("org rename to %s is refused: this organization has images in the registry under %q", want, org.Slug)
		}
		org.Name, org.Slug = f.Org, want
		changed = true
	}
	// defaults: lands on the org row so a stack apply can fall back to it
	// without loading the org file. Written before the stacks apply below.
	if org.UIEditsDefault != f.Defaults.UIEdits {
		org.UIEditsDefault = f.Defaults.UIEdits
		changed = true
	}
	if want := envColorsJSON(f.Defaults.EnvColors); org.EnvColors != want {
		org.EnvColors = want
		changed = true
	}
	// The org's rung of the defaults cascade. Declaring nothing leaves the
	// panel's overrides alone; the file owns what it declares.
	settingsChanged := false
	if !f.Defaults.Empty() {
		if want := f.Defaults.SettingsJSON(); org.Settings != want {
			org.Settings = want
			changed, settingsChanged = true, true
		}
	}
	if changed {
		if err := r.Store.UpdateOrg(ctx, org); err != nil {
			return err
		}
	}
	// Protection feeds the rendered routes.
	if settingsChanged {
		r.Applier.Ops.PX.Resync(ctx)
	}

	// Vars + generated secrets.
	for _, name := range sortedKeys(f.Vars) {
		if err := r.Applier.Ops.Vars.Upsert(ctx, &repo.Variable{OwnerKind: repo.OwnerOrg, OwnerID: org.ID,
			Name: name, Value: f.Vars[name], CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		audit.Record(ctx, r.Store, "config:"+org.Slug, audit.Set, repo.OwnerOrg, org.ID, name)
		deploy.ClearWaitingOrg(ctx, r.Store, r.Applier.Ops.Tiles, org.ID, name)
	}
	cur, _ := r.Store.ListVariables(ctx, repo.OwnerOrg, org.ID)
	set := map[string]bool{}
	for _, v := range cur {
		set[v.Name] = true
	}
	for _, name := range sortedKeys(f.Secrets) {
		sc := f.Secrets[name]
		if sc.Default != "generated" || set[name] {
			continue
		}
		if err := r.Applier.Ops.Vars.Upsert(ctx, &repo.Variable{OwnerKind: repo.OwnerOrg, OwnerID: org.ID,
			Name: name, Secret: true, Value: secrets.Generate(sc.GenLength(), sc.IncludeNumbers, sc.IncludeSymbols),
			CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		// A generated secret is a value nobody chose and nobody can read back
		// off the file: the audit row is the only record it was ever minted.
		audit.Record(ctx, r.Store, "config:"+org.Slug, audit.Set, repo.OwnerOrg, org.ID, name)
		deploy.ClearWaitingOrg(ctx, r.Store, r.Applier.Ops.Tiles, org.ID, name)
	}

	// Org domain resources before the stacks: a stack's own auto-hostname
	// claims resolve against the visible set, and the org's rows are part of it.
	if err := r.applyDomains(ctx, org, f); err != nil {
		return err
	}

	// Shares before the stacks: their tiles mount them on deploy.
	var failed []string
	if err := r.applyStorage(ctx, org, f, &failed); err != nil {
		return err
	}

	// Stacks first, shared instances need a stack env to host their row. A
	// stack naming another stack's middleware fails when it sorts before its
	// provider on a fresh org; the second pass finds the provider applied.
	var again []string
	for _, name := range sortedKeys(f.Stacks) {
		if err := r.applyStack(ctx, org, name, f.Stacks[name]); err != nil {
			if strings.Contains(err.Error(), "no middleware ") {
				again = append(again, name)
				continue
			}
			failed = append(failed, fmt.Sprintf("stack %s: %v", name, err))
		}
	}
	for _, name := range again {
		if err := r.applyStack(ctx, org, name, f.Stacks[name]); err != nil {
			failed = append(failed, fmt.Sprintf("stack %s: %v", name, err))
		}
	}
	// Stack deletions (config-managed, absent from the file, not protected,
	// protection lived in the file that declared them, so absence means the
	// operator already removed the flag too; the manual approve is the gate).
	stacks, _ := r.Store.ListStacksByOrg(ctx, org.ID)
	declared := declaredSlugs(f)
	for i := range stacks {
		s := &stacks[i]
		if declared[s.Slug] || !s.ConfigManaged() {
			continue
		}
		// Same as the panel's delete: the services go and the pooled overlays
		// are handed back by name before the rows that name them. A stack
		// that will not tear down keeps its rows and is reported, rather than
		// leaving a claimed network with services still on it.
		if err := r.Applier.Ops.TeardownStack(ctx, s); err != nil {
			failed = append(failed, fmt.Sprintf("delete stack %s: %v", s.Slug, err))
			continue
		}
		if err := r.Store.DeleteStack(ctx, s.ID); err != nil {
			failed = append(failed, fmt.Sprintf("delete stack %s: %v", s.Slug, err))
		}
	}

	// Shared org-scoped instances.
	if err := r.applyShared(ctx, org, f, &failed); err != nil {
		return err
	}

	if len(failed) > 0 {
		msg := strings.Join(failed, "; ")
		_ = r.Store.SetOrgConfigPlanError(ctx, cp.ID, msg)
		return fmt.Errorf("%s", msg)
	}
	return r.Store.SetOrgConfigPlanStatus(ctx, cp.ID, "applied")
}

// applyDomains reconciles the org's domain resources with the file. Deletions
// run last so a host moving between entries never frees its row early.
func (r Runner) applyDomains(ctx context.Context, org *repo.Org, f *File) error {
	all, err := r.Store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	own := map[string]repo.DomainResource{}
	for _, res := range all {
		if res.Level == "org" && res.OwnerID == org.ID {
			own[res.Host] = res
		}
	}
	declared := map[string]bool{}
	for _, d := range f.Domains {
		declared[d.Host] = true
		cur, ok := own[d.Host]
		if !ok {
			if err := r.Store.CreateDomainResource(ctx, &repo.DomainResource{
				ID: uuid.New().String(), Level: "org", OwnerID: org.ID,
				Host: d.Host, IncludeEnvOnDefault: d.IncludeEnvOnDefault,
				ACMEEmail: d.ACMEEmail, Declared: true, CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
			continue
		}
		// Naming a panel row here adopts it; only then can a later apply
		// delete it.
		if cur.IncludeEnvOnDefault != d.IncludeEnvOnDefault || !cur.Declared {
			cur.IncludeEnvOnDefault = d.IncludeEnvOnDefault
			cur.Declared = true
			if err := r.Store.UpdateDomainResource(ctx, &cur); err != nil {
				return err
			}
		}
		// Separately, and through the service: the ACME account lives in
		// traefik's static config, so changing it needs a restart. This path
		// wrote the row and never restarted, although the plan line it emits
		// promises "traefik restarts once".
		if cur.ACMEEmail != d.ACMEEmail && r.Resources != nil {
			if err := r.Resources.SetACME(ctx, cur.ID, d.ACMEEmail); err != nil {
				return err
			}
			cur.ACMEEmail = d.ACMEEmail
		}
	}
	for host, res := range own {
		if !declared[host] && res.Declared {
			if err := r.Store.DeleteDomainResource(ctx, res.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// applyStack creates/binds one declared stack and hands the rest to the
// stack-level machinery.
func (r Runner) applyStack(ctx context.Context, org *repo.Org, name string, ref StackRef) error {
	slug := repo.Slugify(name)
	s, err := r.Store.GetStackBySlug(ctx, org.ID, slug)
	if err != nil {
		return err
	}
	if s == nil {
		// Through the service: this copy built the row and the production
		// environment by hand, so it had none of the rules the panel and the
		// API go through, and its environment had none either.
		if s, err = r.StackSvc.Create(ctx, org.ID, service.CreateStack{Name: name, OrgDeclared: true}); err != nil {
			return err
		}
	}
	if ref.form() == "inline" {
		b, err := yaml.Marshal(ref.Inline)
		if err != nil {
			return err
		}
		resolved, err := stackconf.Load(b, func(string) ([]byte, error) {
			return nil, fmt.Errorf("inline stacks cannot use the include: key")
		})
		if err != nil {
			return err
		}
		// Opts rather than a bare DiffOpts: an inline stack claims hosts like
		// any other, so it has to carry the same domain context, including the
		// other orgs' slugs its literal hosts may not lead with.
		ok, err := r.Applier.ApplyResolved(ctx, s, resolved, r.Stacks.Opts(ctx, s, "", "", nil), true)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("inline apply did not run")
		}
		return nil
	}
	wantRepo, wantConn, wantBranch, wantPath := r.refBinding(org, ref)
	if s.ConfigRepo != wantRepo || s.ConfigConnectorID != wantConn || s.ConfigBranch != wantBranch || s.ConfigPath != wantPath || !s.OrgDeclared {
		// Through the service: this copy wrote the binding with none of the
		// three things the panel's own bind does. It took the connector id on
		// trust, where a connector is a credential and only the stack's own
		// org may lend one; it stored the repo string unnormalised, so a
		// github URL and an owner/name for the same repo were two different
		// bindings; and it left every environment's staged rows in place,
		// where the file now owns the stack and applying them would be a
		// structural write the lock exists to block.
		if err := r.StackSvc.Bind(ctx, s, service.BindConfig{
			ConnectorID: wantConn, Repo: wantRepo, Branch: wantBranch, Path: wantPath,
			OrgDeclared: true,
		}); err != nil {
			return err
		}
	}
	// The stack planner takes it from here (its own plan, its own policy).
	_, err = r.Stacks.RunAll(ctx, s, "")
	if err != nil && err != stackconf.ErrNoFile && err != stackconf.ErrNotBound {
		return err
	}
	return nil
}

// applyShared reconciles org-scoped managed instances. New rows are hosted in
// the org's first stack's first environment, the org file is the owner, the
// hosting row is an implementation detail.
func (r Runner) applyShared(ctx context.Context, org *repo.Org, f *File, failed *[]string) error {
	curTiles, err := r.orgTiles(ctx, org)
	if err != nil {
		return err
	}
	bySlug := map[string]*repo.Tile{}
	for i := range curTiles {
		bySlug[curTiles[i].Slug] = &curTiles[i]
	}
	for _, name := range sortedKeys(f.Shared) {
		tc, err := f.sharedConf(name)
		if err != nil {
			return err
		}
		t, exists := bySlug[name]
		if exists && t.Engine == tc.Engine {
			def := managedtiles.Engines[t.Engine].DefaultImage
			want := defStr(tc.Image, def)
			changed := t.ImageRef != want || t.ShmSizeMB != tc.ShmSizeMB || t.ExternalPort != tc.ExternalPort
			if !changed {
				continue
			}
			t.ImageRef, t.ShmSizeMB, t.ExternalPort = want, tc.ShmSizeMB, tc.ExternalPort
			t.UpdatedAt = time.Now().UTC()
			if err := r.Store.UpdateTile(ctx, t.ID, t.TileConfig); err != nil {
				return err
			}
			if r.Applier.Instances != nil && (t.Status == "running" || t.Status == "error") {
				// The service writes the status; this path used to redeploy and
				// leave the column saying whatever it said before.
				if err := r.Applier.Instances.Deploy(ctx, t); err != nil {
					*failed = append(*failed, fmt.Sprintf("shared %s: redeploy: %v", name, err))
				}
			}
			continue
		}
		if exists { // engine replace: drop, then recreate below
			// The full teardown, not just the service: this path released the
			// network pool and nothing else, so the route, the slices cut from
			// the instance and their resource rows all outlived it.
			if err := r.teardownShared(ctx, t); err != nil {
				return err
			}
		}
		env, herr := r.hostEnv(ctx, org)
		if herr != nil {
			*failed = append(*failed, fmt.Sprintf("shared %s: %v", name, herr))
			continue
		}
		now := time.Now().UTC()
		t = &repo.Tile{ID: uuid.New().String(), StackID: env.StackID, EnvironmentID: env.ID,
			Name: name, Slug: name, Engine: tc.Engine, SourceType: "image", Kind: "service",
			ImageRef: tc.Image, ShmSizeMB: tc.ShmSizeMB, ExternalPort: tc.ExternalPort,
			ScopeKind: "org", ScopeID: org.ID,
			WebhookToken: uuid.New().String(), Status: "idle", CreatedAt: now, UpdatedAt: now}
		if err := managedtiles.NewDB(t); err != nil {
			return err
		}
		if err := r.Store.CreateTile(ctx, t); err != nil {
			return err
		}
		managedtiles.PublishConnection(ctx, r.Store, t)
		if r.Applier.Instances != nil {
			if err := r.Applier.Instances.Deploy(ctx, t); err != nil {
				*failed = append(*failed, fmt.Sprintf("shared %s: deploy: %v", name, err))
			}
		}
	}
	for slug, t := range bySlug {
		if _, declared := f.Shared[slug]; declared {
			continue
		}
		if err := r.teardownShared(ctx, t); err != nil {
			*failed = append(*failed, fmt.Sprintf("shared %s: delete: %v", slug, err))
		}
	}
	return nil
}

// teardownShared removes an org-scoped instance completely. The file no
// longer declares it, so the held-slices refusal is answered by the file
// itself: force.
func (r Runner) teardownShared(ctx context.Context, t *repo.Tile) error {
	if r.Applier.Instances == nil {
		return r.Store.DeleteTile(ctx, t.ID)
	}
	return r.Applier.Instances.TearDown(ctx, t, true)
}

// hostEnv picks where an org-scoped instance row physically lives: the first
// stack's first environment. arbitrary but stable; a dedicated org
// infra stack can replace this when it matters.
func (r Runner) hostEnv(ctx context.Context, org *repo.Org) (*repo.Environment, error) {
	stacks, err := r.Store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	if len(stacks) == 0 {
		return nil, fmt.Errorf("org has no stacks to host the instance; declare at least one stack")
	}
	sort.Slice(stacks, func(i, j int) bool { return stacks[i].CreatedAt.Before(stacks[j].CreatedAt) })
	envs, err := r.Store.ListEnvironmentsByStack(ctx, stacks[0].ID)
	if err != nil || len(envs) == 0 {
		return nil, fmt.Errorf("stack %s has no environments", stacks[0].Slug)
	}
	return &envs[0], nil
}

// envColorsJSON is the org row's form of defaults.env_colors; "" when the
// file sets none, so an org without the key does not churn the row.
func envColorsJSON(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func defStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
