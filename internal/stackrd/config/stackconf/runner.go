package stackconf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ErrNotBound marks a stack without a config binding.
var ErrNotBound = errors.New("stack has no config repo bound")

// ErrNoFile marks a bound stack whose repo has no config file, by design a
// no-op, not an error state.
var ErrNoFile = errors.New("config file not found in repo")

// FileSource fetches files from the bound repo (implemented by githubapp).
type FileSource interface {
	FileContents(ctx context.Context, cn *repo.Connector, repoFull, ref, path string) ([]byte, error)
	DefaultBranch(ctx context.Context, cn *repo.Connector, repoFull string) (string, error)
	HeadSHA(ctx context.Context, cn *repo.Connector, repoFull, ref string) (string, error)
}

// Planner loads a stack's config from its bound repo, diffs it against the
// database, and stores the result as a ConfigPlan.
type Planner struct {
	Store repo.Store
	Src   FileSource
}

// StackBranch resolves the branch a stack-scoped plan reads from.
func (pl Planner) StackBranch(ctx context.Context, stack *repo.Stack) (string, error) {
	if stack.ConfigBranch != "" {
		return stack.ConfigBranch, nil
	}
	cn, err := pl.Store.GetConnector(ctx, stack.ConfigConnectorID)
	if err != nil || cn == nil {
		return "", fmt.Errorf("config connector: %w", err)
	}
	return pl.Src.DefaultBranch(ctx, cn, stack.ConfigRepo)
}

// LoadResolved fetches and resolves the stack's config file at branch.
// Returns ErrNoFile when the file is absent.
func (pl Planner) LoadResolved(ctx context.Context, stack *repo.Stack, branch string) (*Resolved, error) {
	if !stack.ConfigManaged() {
		return nil, ErrNotBound
	}
	cn, err := pl.Store.GetConnector(ctx, stack.ConfigConnectorID)
	if err != nil || cn == nil {
		return nil, fmt.Errorf("config connector: %w", err)
	}
	path := stack.ConfigPath
	if path == "" {
		path = DefaultPath
	}
	data, err := pl.Src.FileContents(ctx, cn, stack.ConfigRepo, branch, path)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", path, err)
	}
	if data == nil {
		return nil, ErrNoFile
	}
	return Load(data, func(inc string) ([]byte, error) {
		b, ferr := pl.Src.FileContents(ctx, cn, stack.ConfigRepo, branch, inc)
		if ferr != nil {
			return nil, ferr
		}
		if b == nil {
			return nil, fmt.Errorf("not found")
		}
		return b, nil
	})
}

// Opts builds the DiffOpts shared by plan and apply. skip/only scope the diff.
func (pl Planner) Opts(ctx context.Context, stack *repo.Stack, branch, onlyEnv string, skip map[string]bool) DiffOpts {
	opts := DiffOpts{
		GitURL:        "https://github.com/" + stack.ConfigRepo,
		Connector:     stack.ConfigConnectorID,
		DefaultBranch: branch,
		OnlyEnv:       onlyEnv,
		SkipEnvs:      skip,
	}
	pl.loadDomainContext(ctx, stack, &opts)
	return opts
}

// loadDomainContext fills the slugs and visible domain resources auto/apex
// claims resolve against.
func (pl Planner) loadDomainContext(ctx context.Context, stack *repo.Stack, opts *DiffOpts) {
	opts.StackSlug = stack.Slug
	if org, err := pl.Store.GetOrg(ctx, stack.OrgID); err == nil && org != nil {
		opts.OrgSlug = org.Slug
	}
	if envs, err := pl.Store.ListEnvironmentsByStack(ctx, stack.ID); err == nil && len(envs) > 0 {
		opts.DefaultEnv = envs[0].Slug
	}
	if all, err := pl.Store.ListDomainResources(ctx); err == nil {
		opts.DomainResources = envops.VisibleDomainResources(all, stack.ID, stack.OrgID)
	}
	if orgs, err := pl.Store.ListOrgs(ctx); err == nil {
		opts.ForeignOrgSlugs = map[string]bool{}
		for _, o := range orgs {
			if o.ID != stack.OrgID {
				opts.ForeignOrgSlugs[o.Slug] = true
			}
		}
	}
}

// BranchBoundEnvs returns the stack's static envs carrying their own config
// branch, as slug → branch.
func (pl Planner) BranchBoundEnvs(ctx context.Context, stack *repo.Stack) (map[string]string, error) {
	envs, err := pl.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range envs {
		if e.Type == "static" && e.ConfigBranch != "" {
			out[e.Slug] = e.ConfigBranch
		}
	}
	return out, nil
}

// Run plans the config at the stack's bound branch and stores one row: one
// commit, one plan, covering the whole stack and every env it declares. Envs
// bound to their own branch are skipped, RunEnv covers those. sha (optional)
// records which push triggered it.
//
// It used to write one row per rung, the stack-scoped row carrying the bottom
// rung and every env above it getting its own. That made a config plan a
// promotion rung, which it is not: a plan is one diff of one commit, and
// moving an image up the ladder is a separate thing that lives on the
// releases page (docs/plans/34-apply-and-review-fixes.md, decision 1).
func (pl Planner) Run(ctx context.Context, stack *repo.Stack, sha string) ([]*repo.ConfigPlan, error) {
	if !stack.ConfigManaged() {
		return nil, ErrNotBound
	}
	branch, err := pl.StackBranch(ctx, stack)
	if err != nil {
		return nil, fmt.Errorf("default branch: %w", err)
	}
	bound, err := pl.BranchBoundEnvs(ctx, stack)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for slug, b := range bound {
		if b != branch {
			skip[slug] = true
		}
	}
	sha, resolved, errPlan, err := pl.load(ctx, stack, sha, branch)
	if err != nil {
		return nil, err
	}
	if errPlan != nil {
		cp, serr := pl.save(ctx, stack, sha, errPlan)
		return []*repo.ConfigPlan{cp}, serr
	}
	state, err := pl.Snapshot(ctx, stack)
	if err != nil {
		return nil, err
	}
	cp, err := pl.diffSave(ctx, stack, sha, branch, resolved, state, "", skip)
	if err != nil {
		return nil, err
	}
	return []*repo.ConfigPlan{cp}, nil
}

// RunAll is "re-plan this stack": the stack-scoped plan plus a plan for every
// env pinned to its own branch, which Run deliberately skips. Every plan it
// produced comes back, stack-scoped one first, a caller that reports only the
// first would tell someone "one change" while a branch-bound staging env has a
// deletion queued. A failure on one env does not sink the rest.
//
// Not the webhook's path: a push re-plans only what that branch drives, which
// is a different (narrower) set. This is the "re-plan everything" button, used
// by the panel, the API, and the post-secret-change refresh.
func (pl Planner) RunAll(ctx context.Context, stack *repo.Stack, sha string) ([]*repo.ConfigPlan, error) {
	out, err := pl.Run(ctx, stack, sha)
	if err != nil {
		return nil, err
	}
	envs, lerr := pl.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if lerr != nil {
		return out, nil
	}
	for i := range envs {
		if envs[i].Type != "static" || envs[i].ConfigBranch == "" {
			continue
		}
		if cp, rerr := pl.RunEnv(ctx, stack, &envs[i], sha); rerr == nil && cp != nil {
			out = append(out, cp)
		}
	}
	return out, nil
}

// RunEnv produces and stores a plan scoped to one env, read from that env's
// own config branch.
func (pl Planner) RunEnv(ctx context.Context, stack *repo.Stack, env *repo.Environment, sha string) (*repo.ConfigPlan, error) {
	if !stack.ConfigManaged() {
		return nil, ErrNotBound
	}
	if env.ConfigBranch == "" {
		return nil, fmt.Errorf("env %s has no config branch", env.Slug)
	}
	return pl.plan(ctx, stack, sha, env.ConfigBranch, env.Slug, nil)
}

func (pl Planner) plan(ctx context.Context, stack *repo.Stack, sha, branch, onlyEnv string, skip map[string]bool) (*repo.ConfigPlan, error) {
	sha, resolved, errPlan, err := pl.load(ctx, stack, sha, branch)
	if err != nil {
		return nil, err
	}
	if errPlan != nil {
		errPlan.EnvSlug = onlyEnv
		return pl.save(ctx, stack, sha, errPlan)
	}
	state, err := pl.Snapshot(ctx, stack)
	if err != nil {
		return nil, err
	}
	return pl.diffSave(ctx, stack, sha, branch, resolved, state, onlyEnv, skip)
}

// load resolves the sha and the config for a plan. A config that does not
// load comes back as an error row to store, not an error.
func (pl Planner) load(ctx context.Context, stack *repo.Stack, sha, branch string) (string, *Resolved, *repo.ConfigPlan, error) {
	// A push supplies the sha. A hand-triggered plan doesn't, and without one
	// apply falls back to the branch tip, so the panel's own plan → approve →
	// apply flow could still apply a different commit than the one reviewed.
	if sha == "" {
		if cn, cerr := pl.Store.GetConnector(ctx, stack.ConfigConnectorID); cerr == nil && cn != nil {
			sha, _ = pl.Src.HeadSHA(ctx, cn, stack.ConfigRepo, branch)
		}
	}
	resolved, err := pl.LoadResolved(ctx, stack, branch)
	if err == ErrNoFile || err == ErrNotBound {
		return sha, nil, nil, err
	}
	if err != nil {
		return sha, nil, &repo.ConfigPlan{Status: "error", Error: err.Error(), Summary: "config invalid"}, nil
	}
	return sha, resolved, nil, nil
}

// diffSave diffs one scope of a loaded config against state and stores the
// row.
func (pl Planner) diffSave(ctx context.Context, stack *repo.Stack, sha, branch string, resolved *Resolved, state State, onlyEnv string, skip map[string]bool) (*repo.ConfigPlan, error) {
	if onlyEnv != "" {
		if _, ok := resolved.Envs[onlyEnv]; !ok {
			return pl.save(ctx, stack, sha, &repo.ConfigPlan{EnvSlug: onlyEnv, Status: "error",
				Error: fmt.Sprintf("environment %q is not declared in the config on branch %s", onlyEnv, branch), Summary: "config invalid"})
		}
	}
	plan := Diff(resolved, state, pl.Opts(ctx, stack, branch, onlyEnv, skip))
	pl.finishPlan(ctx, stack, resolved, plan, onlyEnv, skip)
	blob, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	status := "pending"
	if plan.Empty() {
		status = "clean"
	}
	return pl.save(ctx, stack, sha, &repo.ConfigPlan{EnvSlug: onlyEnv, Status: status, Summary: plan.Summary(), Plan: string(blob)})
}

func (pl Planner) save(ctx context.Context, stack *repo.Stack, sha string, p *repo.ConfigPlan) (*repo.ConfigPlan, error) {
	if err := pl.Store.SupersedePendingPlans(ctx, stack.ID, p.EnvSlug); err != nil {
		return nil, err
	}
	p.ID = uuid.New().String()
	p.StackID = stack.ID
	p.CommitSHA = sha
	p.CreatedAt = time.Now().UTC()
	if err := pl.Store.CreateConfigPlan(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Preview computes a plan without storing it, used for PR comments. The
// config file is read from fetchBranch (the PR head) but diffed as if it were
// applied from applyBranch (the merge target), so tile-branch fallbacks don't
// show phantom changes. onlyEnv/skip scope exactly like the stored-plan paths.
func (pl Planner) Preview(ctx context.Context, stack *repo.Stack, fetchBranch, applyBranch, onlyEnv string, skip map[string]bool) (*Plan, error) {
	resolved, err := pl.LoadResolved(ctx, stack, fetchBranch)
	if err != nil {
		return nil, err
	}
	if onlyEnv != "" {
		if _, ok := resolved.Envs[onlyEnv]; !ok {
			return nil, fmt.Errorf("environment %q is not declared in the config on branch %s", onlyEnv, fetchBranch)
		}
	}
	state, err := pl.Snapshot(ctx, stack)
	if err != nil {
		return nil, err
	}
	plan := Diff(resolved, state, pl.Opts(ctx, stack, applyBranch, onlyEnv, skip))
	pl.finishPlan(ctx, stack, resolved, plan, onlyEnv, skip)
	return plan, nil
}

// PreviewBundle computes a plan from a posted config bundle without storing it,
// the CLI's local dry-run. The stack must be bound: the diff runs as if the
// bundle were applied from the stack's bound branch, so tile-branch fallbacks
// match a real apply. Envs pinned to their own branch are skipped with a
// warning (their plan comes from that branch), unless onlyEnv names one.
// Unlike Preview, this merges SecretIssues/GenSecrets, same set as plan().
func (pl Planner) PreviewBundle(ctx context.Context, stack *repo.Stack, main []byte, files map[string][]byte, onlyEnv string) (*Plan, error) {
	if !stack.ConfigManaged() {
		return nil, ErrNotBound
	}
	branch, err := pl.StackBranch(ctx, stack)
	if err != nil {
		return nil, fmt.Errorf("default branch: %w", err)
	}
	resolved, err := Load(main, func(inc string) ([]byte, error) {
		if b, ok := files[inc]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("not in the posted bundle")
	})
	if err != nil {
		return nil, err
	}
	if onlyEnv != "" {
		if _, ok := resolved.Envs[onlyEnv]; !ok {
			return nil, fmt.Errorf("environment %q is not declared in the config", onlyEnv)
		}
	}
	var skip map[string]bool
	var skipWarns []string
	if onlyEnv == "" {
		bound, berr := pl.BranchBoundEnvs(ctx, stack)
		if berr != nil {
			return nil, berr
		}
		skip = map[string]bool{}
		for slug, b := range bound {
			if b != branch {
				skip[slug] = true
				skipWarns = append(skipWarns, fmt.Sprintf("env %s tracks branch %s and is not previewed; preview it with --env %s", slug, b, slug))
			}
		}
		sort.Strings(skipWarns)
	}
	state, err := pl.Snapshot(ctx, stack)
	if err != nil {
		return nil, err
	}
	plan := Diff(resolved, state, pl.Opts(ctx, stack, branch, onlyEnv, skip))
	pl.finishPlan(ctx, stack, resolved, plan, onlyEnv, skip)
	plan.Warnings = append(plan.Warnings, skipWarns...)
	return plan, nil
}

// finishPlan runs the shared post-Diff phase: the stack-rename check, the
// bad-ref gate and the secret issues. Every path that shows a plan to a human
// runs the same set, a preview that skipped one would claim clean where the
// stored plan errors.
func (pl Planner) finishPlan(ctx context.Context, stack *repo.Stack, resolved *Resolved, plan *Plan, onlyEnv string, skip map[string]bool) {
	if onlyEnv == "" {
		pl.planRename(ctx, stack, resolved, plan)
	}
	pl.planVars(ctx, stack, resolved, plan, onlyEnv, skip)
	pl.planBackups(ctx, stack, resolved, plan, onlyEnv, skip)
	plan.Errors = append(plan.Errors, pl.BadRefs(ctx, stack, resolved, onlyEnv, skip)...)
	secretErrs, secretWarns, gen := pl.SecretIssues(ctx, stack, resolved, onlyEnv, skip)
	plan.Errors = append(plan.Errors, secretErrs...)
	plan.Warnings = append(plan.Warnings, secretWarns...)
	plan.GenSecrets = gen
	plan.Inputs = pl.DeclaredInputs(ctx, stack, resolved, onlyEnv, skip)
	// Declared values are rows, not banners: the reviewer reads one list.
	for _, in := range plan.Inputs {
		plan.Changes = append(plan.Changes, InputChange(in))
	}
	for _, name := range gen {
		plan.Changes = append(plan.Changes, GenChange("stack", name))
	}
	plan.InputsFirst()
}

// planRename adds the stack-rename change when the file's stack: disagrees
// with the row. Stack identity rides the config binding (same design as the
// org's org: field), so stack: is desired state, a mismatch plans a rename.
// Stack-scoped plans only: an env-branch plan reads another branch's file,
// which has no say over the stack's name.
func (pl Planner) planRename(ctx context.Context, stack *repo.Stack, r *Resolved, p *Plan) {
	want := repo.Slugify(r.Stack)
	if want == "" || want == stack.Slug {
		return
	}
	// An org-declared stack is named by the org file's stacks: key, the
	// instantiator names the instance (Helm-style), so the stack file cannot
	// rename it. Error, not a silent skip: the mismatch would otherwise sit
	// invisible forever. Gated on the org still being config-managed so a
	// stale flag on an unbound org never wedges renames.
	if stack.OrgDeclared {
		if org, err := pl.Store.GetOrg(ctx, stack.OrgID); err == nil && org != nil && org.ConfigManaged() {
			p.Errors = append(p.Errors, "stack: "+r.Stack+": the org config file owns this stack's name ("+stack.Slug+"); rename its key there, or remove the stack from the org file first")
			return
		}
	}
	if other, err := pl.Store.GetStackBySlug(ctx, stack.OrgID, want); err == nil && other != nil && other.ID != stack.ID {
		p.Errors = append(p.Errors, "stack rename to "+want+" collides with an existing stack")
		return
	}
	p.Changes = append(p.Changes, Change{Kind: "update", Env: "stack", Field: "slug",
		Old: stack.Slug, New: want,
		Note: "renames the stack: URLs, CLI paths and container names change; every running tile is stopped and redeployed. An org file declaring this stack must rename its key too"})
}

// planVars diffs the file's vars: against the stored stack- and env-owner rows.
// Additive and update only: nothing records where a variable came from, so a
// name the file does not declare may just as well be a panel edit, and deleting
// it would quietly take out a value the file never claimed.
//
// A name already stored as a secret is an error rather than a silent overwrite:
// the file would be putting a credential in git.
func (pl Planner) planVars(ctx context.Context, stack *repo.Stack, r *Resolved, p *Plan, onlyEnv string, skip map[string]bool) {
	add := func(scope, name, old, want string) {
		if old == want {
			return
		}
		kind := "update"
		if old == "" {
			kind = "create"
		}
		p.Changes = append(p.Changes, Change{Kind: kind, Env: scope, Tile: "vars", Field: name,
			Old: old, New: want, Note: "read as ${{ " + scopeRef(scope) + ".vars." + name + " }}"})
	}
	load := func(ownerKind, ownerID string) (map[string]string, map[string]bool) {
		vals, secret := map[string]string{}, map[string]bool{}
		vars, err := pl.Store.ListVariables(ctx, ownerKind, ownerID)
		if err != nil {
			return vals, secret
		}
		for _, v := range vars {
			vals[v.Name], secret[v.Name] = v.Value, v.Secret
		}
		return vals, secret
	}
	check := func(scope, name string, secret map[string]bool) bool {
		if !secret[name] {
			return true
		}
		p.Errors = append(p.Errors, "var "+name+" is a secret on the "+scope+"; declare it under secrets:, not vars:")
		return false
	}
	if onlyEnv == "" && len(r.Vars) > 0 {
		vals, secret := load(repo.OwnerStack, stack.ID)
		for _, name := range sortedMapKeys(r.Vars) {
			if check("stack", name, secret) {
				add("stack", name, vals[name], r.Vars[name])
			}
		}
	}
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] || len(r.Envs[envName].Vars) == 0 {
			continue
		}
		env, err := pl.Store.GetEnvironmentBySlug(ctx, stack.ID, envName)
		if err != nil || env == nil {
			// Not created yet: the create-env row already says the env is new,
			// and the apply writes these once it exists.
			for _, name := range sortedMapKeys(r.Envs[envName].Vars) {
				add(envName, name, "", r.Envs[envName].Vars[name])
			}
			continue
		}
		vals, secret := load(repo.OwnerEnv, env.ID)
		for _, name := range sortedMapKeys(r.Envs[envName].Vars) {
			if check("env "+envName, name, secret) {
				add(envName, name, vals[name], r.Envs[envName].Vars[name])
			}
		}
	}
}

// scopeRef is the reference scope a plan row's Env column belongs to: an env
// row still shadows a stack variable, so it is read through stack.
func scopeRef(scope string) string {
	if scope == "org" {
		return "org"
	}
	return "stack"
}

// SecretIssues checks declared secrets against stored values. One thing errors:
// an env override on a secret pinned stack-wide (env_versions: false), refused
// loudly rather than silently ignored, because the file contradicts itself.
// Everything missing only warns and comes back as an Input for the plan page to
// collect, including `required`, which says the stack needs the value to run,
// not that it may not be created. A stack has to be applicable before its
// credentials exist, or you could never stand one up from an empty panel.
//
// Values are never checked for content and never printed. The file declares
// only that a name must exist.
func (pl Planner) SecretIssues(ctx context.Context, stack *repo.Stack, r *Resolved, onlyEnv string, skip map[string]bool) (errs, warns, gen []string) {
	set, err := pl.setVarNames(ctx, stack)
	if err != nil {
		// A store blip must not read as "everything is configured".
		return nil, []string{fmt.Sprintf("could not check declared secrets: %v", err)}, nil
	}
	errSet, warnSet, genSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		envSet := map[string]bool{}
		if env, err := pl.Store.GetEnvironmentBySlug(ctx, stack.ID, envName); err == nil && env != nil {
			if vars, err := pl.Store.ListVariables(ctx, repo.OwnerEnv, env.ID); err == nil {
				for _, v := range vars {
					envSet[v.Name] = true
				}
			}
		}
		for name, sc := range r.Envs[envName].Secrets {
			if !sc.PerEnv() && envSet[name] {
				errSet[fmt.Sprintf("secret %s is stack-wide (env_versions: false) but env %s holds its own value; remove the env value or drop env_versions: false", name, envName)] = true
			}
			if set[name] || (sc.PerEnv() && envSet[name]) {
				continue
			}
			if sc.Default == "generated" {
				genSet[name] = true
			}
		}
	}
	return sortedBoolKeys(errSet), sortedBoolKeys(warnSet), sortedBoolKeys(genSet)
}

// DeclaredInputs is the declared secrets nobody has set, minus the generated
// ones the apply mints on its own, the boxes the plan page offers. Same walk
// as SecretIssues, kept separate so its four callers stay unchanged.
func (pl Planner) DeclaredInputs(ctx context.Context, stack *repo.Stack, r *Resolved, onlyEnv string, skip map[string]bool) []Input {
	set, err := pl.setVarNames(ctx, stack)
	if err != nil {
		return nil
	}
	// One Input per name, but the blocked list accumulates across every env
	// the name is unset in, a value missing in two envs stops tiles in both.
	seen := map[string]*Input{}
	var refs []*Input
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		envSet := map[string]bool{}
		if env, err := pl.Store.GetEnvironmentBySlug(ctx, stack.ID, envName); err == nil && env != nil {
			if vars, err := pl.Store.ListVariables(ctx, repo.OwnerEnv, env.ID); err == nil {
				for _, v := range vars {
					envSet[v.Name] = true
				}
			}
		}
		re := r.Envs[envName]
		for name, sc := range re.Secrets {
			if sc.Default == "generated" || set[name] || (sc.PerEnv() && envSet[name]) {
				continue
			}
			in, ok := seen[name]
			if !ok {
				in = &Input{Scope: "stack", Name: name, Secret: true, Required: sc.Required}
				seen[name] = in
				refs = append(refs, in)
			}
			for _, tile := range blockedTiles(re, name) {
				in.Blocked = append(in.Blocked, envName+"/"+tile)
			}
		}
	}
	out := make([]Input, 0, len(refs))
	for _, in := range refs {
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// blockedTiles is every tile in the env that will not come up while name has
// no value: the tiles that read it, plus everything that depends on those,
// transitively. The chain matters because a tile parked as waiting never
// satisfies a depends_on, so the deploy engine parks its dependents too.
func blockedTiles(re ResolvedEnv, name string) []string {
	blocked := map[string]bool{}
	for tileName, tc := range re.Tiles {
		for _, val := range tc.Env {
			for _, body := range varref.Refs(val) {
				ref, err := varref.Parse(body)
				if err == nil && ref.IsBucket() && ref.Name == name && (ref.Scope == "stack" || ref.Scope == "org") {
					blocked[tileName] = true
				}
			}
		}
	}
	// Closure over depends_on. Bounded: each pass adds at least one tile or
	// stops, so at most len(Tiles) passes over a handful of tiles.
	for grew := true; grew; {
		grew = false
		for tileName, tc := range re.Tiles {
			if blocked[tileName] {
				continue
			}
			for _, line := range tc.DependsOn {
				if slug, _, err := ParseDep(line); err == nil && blocked[slug] {
					blocked[tileName] = true
					grew = true
				}
			}
		}
	}
	return sortedBoolKeys(blocked)
}

// setVarNames is every variable name resolvable from this stack's tiles by a
// bucket reference (`${{ stack.vars.NAME }}`, `${{ org.secrets.NAME }}`), the
// two scopes a declared secret can live at. Both buckets, the caller only asks
// whether the name has a value at all.
func (pl Planner) setVarNames(ctx context.Context, stack *repo.Stack) (map[string]bool, error) {
	out := map[string]bool{}
	for _, s := range []struct{ kind, id string }{
		{repo.OwnerStack, stack.ID}, {repo.OwnerOrg, stack.OrgID},
	} {
		vars, err := pl.Store.ListVariables(ctx, s.kind, s.id)
		if err != nil {
			return nil, err
		}
		for _, v := range vars {
			out[v.Name] = true
		}
	}
	return out, nil
}

// BadRefs reports ${{ ... }} references the config declares that could not
// resolve after an apply, malformed, pointing at a source the file doesn't
// declare, or naming a stack/org variable nobody has set. Plan errors, because
// applying them produces tiles that refuse to deploy.
//
// Apply calls this too (not just plan): a webhook push plans and applies in one
// go, so a gate that only ran at plan time never ran at all for that path.
func (pl Planner) BadRefs(ctx context.Context, stack *repo.Stack, r *Resolved, onlyEnv string, skip map[string]bool) []string {
	var out []string
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		re := r.Envs[envName]
		known := map[string]bool{}
		for name := range re.Tiles {
			known[name] = true
		}
		// Managed resources aren't declared in the file (they're provisioned),
		// so fold in whatever the env already holds before calling a source
		// unknown. A not-yet-created env has nothing to add.
		if env, err := pl.Store.GetEnvironmentBySlug(ctx, stack.ID, envName); err == nil && env != nil {
			res, err := pl.Store.ListResourcesByEnv(ctx, env.ID)
			if err != nil {
				// Report it rather than skipping the check: a store blip must not
				// turn the gate into a pass.
				out = append(out, fmt.Sprintf("env %s: could not list managed resources: %v", envName, err))
				continue
			}
			for _, rs := range res {
				known[rs.Slug] = true
			}
		}
		// Slice entries are already in re.Tiles under their config key, so a
		// file that both cuts a slice and reads its outputs passes the gate,
		// the resource is published during the same apply.
		// A secret the file declares is not "nobody has set it": the apply
		// mints or creates it, and SecretIssues above already says which.
		// Two gates on one name would soft lock every stack that reads a
		// generated secret.
		declared := map[string]bool{}
		for name := range re.Secrets {
			declared[name] = true
		}
		bad := map[string]bool{}
		for _, tc := range re.Tiles {
			for _, val := range tc.Env {
				for _, body := range varref.Refs(val) {
					if msg := pl.checkRef(ctx, stack, body, known, declared); msg != "" {
						bad[msg] = true
					}
				}
			}
		}
		for _, msg := range sortedBoolKeys(bad) {
			out = append(out, fmt.Sprintf("env %s: %s", envName, msg))
		}
	}
	return out
}

// checkRef validates one reference against post-apply state, returning "" when
// it looks resolvable. Output names aren't checked, they come from the same
// file, from R3's virtual endpoint outputs, or from a resource's provider, so
// only the source and the stack/org variables are knowable here.
func (pl Planner) checkRef(ctx context.Context, stack *repo.Stack, body string, known, declared map[string]bool) string {
	ref, err := varref.Parse(body)
	if err != nil {
		return err.Error()
	}
	switch {
	case ref.Slug == varref.BucketBackups:
		// Before the stackr case: ${{ stackr.backups.X }} is a destination,
		// not a platform value. Destinations are not stack config; the backup
		// planner owns them.
		return ""
	case ref.Scope == "stackr":
		// Parse already rejected any name outside the closed set, and the value
		// itself is not knowable here: the proxy records it when it attaches to
		// the environment's network, and for a new env that network is created
		// by this very plan's apply. Demanding it now would refuse the first
		// deploy of every environment that uses the scope.
		return ""
	case ref.IsBucket():
		want := ref.WantSecret()
		if ref.Scope == "stack" && want && declared[ref.Name] {
			return "" // declared as a secret; the apply makes it exist
		}
		ownerKind, ownerID := repo.OwnerStack, stack.ID
		if ref.Scope == "org" {
			ownerKind, ownerID = repo.OwnerOrg, stack.OrgID
		}
		vars, err := pl.Store.ListVariables(ctx, ownerKind, ownerID)
		if err != nil {
			return fmt.Sprintf("could not check %s: %v", ref.Source, err)
		}
		for _, v := range vars {
			if v.Name != ref.Name {
				continue
			}
			if v.Secret != want {
				other := varref.BucketVars
				if !want {
					other = varref.BucketSecrets
				}
				return fmt.Sprintf("%s: %s is in the %s namespace; write ${{ %s.%s.%s }}",
					ref.Source, ref.Name, other, ref.Scope, other, ref.Name)
			}
			return ""
		}
		return fmt.Sprintf("%s has no value; set it in the %s's %s", ref.Source, ref.Scope, ref.Slug)
	case ref.Scope == "tile":
		if !known[ref.Slug] {
			return fmt.Sprintf("%s: no tile or resource named %q in this environment", ref.Source, ref.Slug)
		}
	}
	return "" // stack/org singletons: resolved per consumer at deploy time
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Snapshot builds the diffable State from the database: static envs and
// their tiles/domains. Ephemeral (PR) envs are never config-managed.
func (pl Planner) Snapshot(ctx context.Context, stack *repo.Stack) (State, error) {
	s := State{Envs: map[string]EnvState{}}
	envs, err := pl.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return s, err
	}
	// The home holds the shared tiles; it is not on the ladder, so it is
	// fetched on its own and diffed under repo.HomeSlug.
	if home, err := pl.Store.HomeEnvironment(ctx, stack.ID); err == nil && home != nil {
		envs = append(envs, *home)
	}
	if cns, err := pl.Store.ListConnectorsByOrg(ctx, stack.OrgID); err == nil {
		s.OrgConnectors = map[string]bool{}
		for i := range cns {
			s.OrgConnectors[cns[i].ID] = true
		}
	}
	apex := map[string]bool{}
	if all, err := pl.Store.ListDomainResources(ctx); err == nil {
		s.AllDomainRes = all
		for _, r := range all {
			if r.Level == "stack" && r.OwnerID == stack.ID {
				s.DomainRes = append(s.DomainRes, r)
			}
		}
		for _, r := range envops.VisibleDomainResources(all, stack.ID, stack.OrgID) {
			apex[r.Host] = true
		}
	}
	if all, err := pl.Store.ListDomains(ctx); err == nil {
		s.AllDomains = all
	}
	if err := pl.middlewareSnapshot(ctx, stack, &s); err != nil {
		return s, err
	}
	paths := map[string]string{} // instance tile id → colon-path, memoised
	for _, env := range envs {
		if env.Type == "ephemeral" {
			continue
		}
		es := EnvState{Tiles: map[string]TileState{}, ApexHosts: apex, Color: env.Color}
		tiles, err := pl.Store.ListTilesByEnv(ctx, env.ID)
		if err != nil {
			return s, err
		}
		for i := range tiles {
			t := tiles[i]
			// Org-scoped instances belong to the ORG config layer (orgconf
			// owns them exclusively; the org file's shared: block declares
			// them). The stack snapshot stays blind to them on purpose,
			// tiles carry no origin column, so an origin-blind snapshot would
			// read every one as a deletion on the stack hosting its row.
			if t.ScopeKind == "org" {
				continue
			}
			domains, err := pl.Store.ListDomainsByTile(ctx, t.ID)
			if err != nil {
				return s, err
			}
			es.Tiles[t.Slug] = TileState{Tile: t, Domains: domains}
		}
		if err := pl.envSlices(ctx, stack, env.ID, es, paths); err != nil {
			return s, err
		}
		s.Envs[env.Slug] = es
	}
	return s, nil
}

// envSlices folds the env's live slices (managed resources + their provision
// rows) into the tile-state map, slices share the tile slug namespace, and
// the differ tells them apart by TileState.Slice.
//
// on_remove comes off the row: the declaration that carried it is gone by the
// time a removal is applied, so apply persists it while the entry still exists.
func (pl Planner) envSlices(ctx context.Context, stack *repo.Stack, envID string, es EnvState, paths map[string]string) error {
	res, err := pl.Store.ListResourcesByEnv(ctx, envID)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return nil
	}
	provisions, err := pl.Store.ListProvisionsByEnv(ctx, envID)
	if err != nil {
		return err
	}
	for i := range res {
		r := &res[i]
		// An orphaned slice (detached, kept data) is not a declaration, it
		// only exists so its data can be reclaimed from the panel.
		var p *repo.Provision
		for j := range provisions {
			if provisions[j].InstanceTileID == r.ProviderTileID && provisions[j].DBName == r.Name && provisions[j].Status != "orphaned" {
				p = &provisions[j]
				break
			}
		}
		if p == nil {
			continue
		}
		inst, err := pl.Store.GetTile(ctx, r.ProviderTileID)
		// A lookup failure must not read as "this env holds nothing", the
		// plan would propose re-adding a slice it already has.
		if err != nil {
			return fmt.Errorf("slice %s: reading providing instance: %w", r.Slug, err)
		}
		if inst == nil {
			continue // instance gone; the resource is dangling, not a declaration
		}
		if _, taken := es.Tiles[r.Slug]; taken {
			continue // a tile owns the slug; varref reports the collision
		}
		path := inst.Slug
		if inst.StackID != stack.ID {
			full, err := pl.infraPath(ctx, inst, paths)
			if err != nil {
				return err
			}
			path = strings.ReplaceAll(full, ":", ".")
		}
		es.Tiles[r.Slug] = TileState{Slice: &SliceState{
			Instance:     inst.Slug,
			Engine:       inst.Engine,
			InstancePath: path,
			Name:         r.Name,
			OnRemove:     p.OnRemove,
			Public:       r.Public,
		}}
	}
	return nil
}

// infraPath addresses a shared instance, memoised per snapshot, several
// consumers usually point at the same handful of instances.
func (pl Planner) infraPath(ctx context.Context, inst *repo.Tile, paths map[string]string) (string, error) {
	if p, ok := paths[inst.ID]; ok {
		return p, nil
	}
	sc, err := envnet.Resolve(ctx, pl.Store, inst)
	if err != nil {
		return "", err
	}
	st, err := pl.Store.GetStack(ctx, inst.StackID)
	if err != nil || st == nil {
		return "", fmt.Errorf("instance %s: stack not found", inst.Slug)
	}
	org, err := pl.Store.GetOrg(ctx, st.OrgID)
	if err != nil || org == nil {
		return "", fmt.Errorf("instance %s: org not found", inst.Slug)
	}
	path := managedtiles.InfraPath(inst.ScopeKind, org.Slug, sc.StackSlug, sc.EnvSlug, inst.Slug)
	paths[inst.ID] = path
	return path, nil
}
