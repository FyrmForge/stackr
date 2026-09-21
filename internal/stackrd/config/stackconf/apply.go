package stackconf

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Applier reconciles the database to a stack's resolved config: the terraform
// promise, after apply, state matches the file. Deploy/schedule/proxy
// side-effects run through the same services the UI uses; each is nil-safe so
// pure-DB reconciliation is unit-testable.
type Applier struct {
	Planner Planner
	Ops     envops.Ops
	DBs     *managedtiles.Service
	// Instances owns a managed instance's deploy/teardown; Slices owns the
	// slices cut from one. Both nil-safe: a pure-DB reconciliation has no
	// cluster behind it.
	Instances *service.ManagedInstanceService
	Slices    *service.SliceService
	// Schedules owns a backup schedule's rules. The file writes through it so
	// a declared schedule gets the same kind derivation, volume guard and
	// validator a panel or API one does; this path had none of the three.
	Schedules *service.BackupScheduleService
	Engine    *deploy.Engine
	// Jobs holds and releases a stack's cron runs around an apply. Schedule
	// reloading goes through Sched, not here.
	Jobs *jobs.Service
	// Sched re-registers the cron and backup tables after the file's blocks
	// land. Nil in tests and wherever an apply is a pure-DB reconciliation.
	Sched *scheduler.Service

	// Deployed, when non-nil, collects the tiles this apply queued a deploy
	// for. The push webhook sets it and skips those tiles in its own branch
	// build: a push that changes both config and code used to queue two
	// builds of the same commit for a tile both paths touched.
	Deployed map[string]bool

	// Per-apply state, set by ApplyPlan on its own copy. upper holds the
	// slugs of envs above the default one: deploys there never build from
	// the branch head, they restart on the current image or promote to
	// commit. Slugs, not ids: the env may be created by this very apply.
	commit   string
	repoURL  string
	upper    map[string]bool
	envSlug  map[string]string // env id -> slug, filled as tiles come by
	deployed map[string]bool
}

// isUpper reports whether a tile lives on an env above the default one.
func (a Applier) isUpper(ctx context.Context, t *repo.Tile) bool {
	slug, ok := a.envSlug[t.EnvironmentID]
	if !ok {
		if env, err := a.Planner.Store.GetEnvironment(ctx, t.EnvironmentID); err == nil && env != nil {
			slug = env.Slug
		}
		if a.envSlug != nil {
			a.envSlug[t.EnvironmentID] = slug
		}
	}
	return a.upper[slug]
}

// deploy queues the build or restart a config change calls for. The default
// env builds from its branch as always. An env above it restarts on the
// image it already runs, or deploys the image at the plan's commit when it has
// no image yet. A config apply never moves an image up the ladder on its own:
// that is a promote, and it is pressed on the releases page.
func (a Applier) deploy(ctx context.Context, t *repo.Tile, what string) {
	// Recorded before the engine check: the set says which tiles this apply
	// owns, which is true whether or not there is an engine to queue on.
	if a.deployed != nil {
		a.deployed[t.ID] = true
	}
	if a.Engine == nil {
		return
	}
	var err error
	switch {
	case !a.isUpper(ctx, t) || t.SourceType != "git" || a.ownRepo(t):
		_, err = a.Engine.Enqueue(ctx, t, "config")
	default:
		var id string
		id, err = a.Engine.EnqueueCurrent(ctx, t, "config")
		if id == "" && err == nil && a.commit != "" {
			_, err = a.Engine.EnqueuePromote(ctx, t, a.commit)
		}
	}
	// Both readers of the set, promoteRest below and the push webhook's
	// autoDeploy, mean "already queued" and skip what it holds. A tile left
	// in it after a failed enqueue is a push that builds nothing at all and
	// says so only in this warning.
	if err != nil && a.deployed != nil {
		delete(a.deployed, t.ID)
	}
	warn("enqueue "+what, t, err)
}

// ownRepo is a tile built from a repo other than the stack's bound one. Its
// image is not a property of the config commit, so it builds from its own
// branch on every env.
func (a Applier) ownRepo(t *repo.Tile) bool {
	return t.GitURL != "" && !strings.EqualFold(strings.TrimSuffix(t.GitURL, ".git"), a.repoURL)
}

// promoteRest deploys the plan commit's image to every git tile in the plan's
// envs that the apply walk did not already queue.
func (a Applier) promoteRest(ctx context.Context, stack *repo.Stack, envSlug string) {
	if a.Engine == nil || a.commit == "" {
		return
	}
	envs, err := a.Planner.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return
	}
	for i := range envs {
		e := &envs[i]
		if e.Type != "static" || !a.upper[e.Slug] || (envSlug != "" && e.Slug != envSlug) {
			continue
		}
		tiles, err := a.Planner.Store.ListTilesByEnv(ctx, e.ID)
		if err != nil {
			continue
		}
		for j := range tiles {
			t := &tiles[j]
			if a.deployed[t.ID] || t.SourceType != "git" || a.ownRepo(t) {
				continue
			}
			if t.Kind != "service" && t.Kind != "cron" && t.Kind != "function" {
				continue
			}
			_, perr := a.Engine.EnqueuePromote(ctx, t, a.commit)
			warn("enqueue promote", t, perr)
		}
	}
}

// Promote deploys the image built at commit to every git tile in one upper
// env, with no config apply behind it. A code-only push produces no config
// change, so there is no plan row to press; the ladder is what decides which
// envs can be promoted to.
//
// The rungs come from the environment list, not from the config file at the
// commit: promoting an image has to keep working while the file does not load
// or a secret it declares is unset.
func (a Applier) Promote(ctx context.Context, stack *repo.Stack, envSlug, commit string) error {
	if commit == "" {
		return fmt.Errorf("promote: no commit")
	}
	envs, err := a.Planner.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return err
	}
	a.upper = map[string]bool{}
	upper, n := false, 0
	for _, e := range envs {
		if e.Type != "static" {
			continue
		}
		if n > 0 {
			a.upper[e.Slug] = true
		}
		if e.Slug == envSlug {
			upper = n > 0
		}
		n++
	}
	if !upper {
		return fmt.Errorf("environment %q is not a rung above the default one", envSlug)
	}
	a.commit = commit
	a.repoURL = "https://github.com/" + strings.TrimSuffix(stack.ConfigRepo, ".git")
	a.promoteRest(ctx, stack, envSlug)
	return nil
}

// warn records a side-effect failure without aborting the apply walk. State is
// already reconciled by the time these run, so the walk must continue, but a
// plan marked "applied" while every deploy failed is a lie, and this is the
// record of it.
// log only. Persist onto the plan row if operators need it in the UI.
func warn(op string, t *repo.Tile, err error) {
	if err == nil {
		return
	}
	if t == nil {
		slog.Error("config apply side-effect failed", "op", op, "error", err)
		return
	}
	// Slug alone repeats across envs and stacks, an operator needs to know
	// which one failed, so carry the ids too.
	slog.Error("config apply side-effect failed", "op", op,
		"tile", t.Slug, "tile_id", t.ID, "env_id", t.EnvironmentID, "stack_id", t.StackID, "error", err)
}

// PolicyFor resolves an env's apply policy. Manual unless someone turned that
// env's own setting to auto, and the file's protected flag ratchets it back to
// manual either way. No env gets auto for free: the bottom rung used to, so a
// push reconciled it unattended, and a config change nobody pressed anything
// for is not something to inherit by position.
func PolicyFor(env *repo.Environment, protected bool) string {
	if protected {
		return "manual"
	}
	if env != nil && env.ApplyPolicy == "auto" {
		return "auto"
	}
	return "manual"
}

// ApplyPlan re-loads the config for the stored plan's scope, re-diffs against
// current state, and reconciles. force=true (human approval) allows deletes
// and ignores per-env policy; force=false is the webhook auto path, it
// applies only when every touched env's policy is auto and nothing is
// destroyed, returning (false, nil) otherwise.
func (a Applier) ApplyPlan(ctx context.Context, stack *repo.Stack, cp *repo.ConfigPlan, force bool) (bool, error) {
	branch, onlyEnv, skip, err := a.scope(ctx, stack, cp)
	if err != nil {
		return false, err
	}
	// Apply the commit that was reviewed, not whatever the branch points at now,
	// otherwise "approved" and "applied" are different files. A SHA is a valid
	// git ref, so this is the same fetch. Opts still gets the branch: that feeds
	// tile git-branch defaults, which must stay a branch name.
	ref := branch
	if cp.CommitSHA != "" {
		ref = cp.CommitSHA
	}
	resolved, err := a.Planner.LoadResolved(ctx, stack, ref)
	if err != nil {
		return false, err
	}
	if skip == nil {
		skip = map[string]bool{}
	}
	// Unattended: apply the envs whose policy is auto and hold the rest for a
	// person. One plan covers the whole stack now, so without this a single
	// manual env (which every env above the bottom rung is by default) would
	// stop a push from applying anything at all.
	held := false
	if !force && onlyEnv == "" {
		held = a.holdManual(ctx, stack, resolved, skip)
	}
	a.commit = cp.CommitSHA
	a.repoURL = "https://github.com/" + strings.TrimSuffix(stack.ConfigRepo, ".git")
	a.deployed = a.Deployed
	if a.deployed == nil {
		a.deployed = map[string]bool{}
	}
	a.envSlug = map[string]string{}
	a.upper = map[string]bool{}
	for _, slug := range resolved.EnvOrder[1:] {
		a.upper[slug] = true
	}
	if onlyEnv != "" {
		if _, ok := resolved.Envs[onlyEnv]; !ok {
			return false, fmt.Errorf("environment %q not declared in config at %s", onlyEnv, ref)
		}
	}
	state, err := a.Planner.Snapshot(ctx, stack)
	if err != nil {
		return false, err
	}
	// Before the diff, so execute sees the same default the diff did.
	opts := withFileDefault(resolved, a.Planner.Opts(ctx, stack, branch, onlyEnv, skip))
	plan := Diff(resolved, state, opts)
	if onlyEnv == "" {
		a.Planner.planRename(ctx, stack, resolved, plan)
	}
	// The same two planners finishPlan runs. Without them a commit that only
	// touches vars: or a backup: block diffs to nothing, plan.Empty() is true
	// below, the plan is recorded as applied and execute, which is what writes
	// the values, never runs. Their rows are stripped again before execute:
	// see the DeleteFunc below.
	a.Planner.planVars(ctx, stack, resolved, plan, onlyEnv, skip)
	a.Planner.planBackups(ctx, stack, resolved, plan, onlyEnv, skip)
	// Re-run the reference check here, not just at plan time: the webhook path
	// plans and applies in one go, so a plan-only gate never gated that flow.
	// Runs before execute, so a bad reference aborts with nothing written.
	plan.Errors = append(plan.Errors, a.Planner.BadRefs(ctx, stack, resolved, onlyEnv, skip)...)
	// Mint generated secrets before the gate: a `default: generated` secret
	// must never block or warn its own apply. Envs not created yet are
	// covered by the second pass inside execute.
	if err := a.ensureSecrets(ctx, stack, resolved, onlyEnv, skip); err != nil {
		return false, err
	}
	// Read back after minting: whatever is still pending here (an env that
	// execute has yet to create) keeps the plan non-empty, so this is never
	// recorded as applied with a secret still missing.
	secretErrs, _, gen := a.Planner.SecretIssues(ctx, stack, resolved, onlyEnv, skip)
	plan.Errors = append(plan.Errors, secretErrs...)
	plan.GenSecrets = gen
	if len(plan.Errors) > 0 {
		return false, fmt.Errorf("plan has errors: %s", strings.Join(plan.Errors, "; "))
	}
	// A pinned tile whose new node group its home node is not in. Applying
	// would deploy it against a volume that is on another machine, so the
	// move has to happen first (docs/plans/31-node-agent-open-questions.md).
	if moves := a.pendingMoves(ctx, plan.Moves); len(moves) > 0 {
		return false, fmt.Errorf("%s hold volumes that have to be moved first", moveNames(moves))
	}
	// A slice whose instance is nowhere cannot be created, and the walk would
	// only find that out ten tiles in, having already made the other ten.
	if err := a.preflightSlices(ctx, stack, resolved, plan); err != nil {
		return false, err
	}
	// ui_edits is policy about the panel, not desired state, so it is not a
	// diffed change. It sits here to satisfy the constraints either side: past
	// the error gate above, so a file that does not load writes nothing (the
	// contract BadRefs states), and ahead of the empty-plan short circuit, so a
	// file that only flips the mode still lands it.
	if err := a.applyUIEdits(ctx, stack, resolved.UIEdits); err != nil {
		return false, err
	}
	if plan.Empty() {
		if held {
			return false, nil
		}
		_ = a.Planner.Store.SetConfigPlanStatus(ctx, cp.ID, "applied")
		return true, nil
	}
	if !force {
		// A rename gates like a destructive change: it changes URLs and restarts
		// every tile, which no unattended webhook push should do. A moved:
		// entry is the same act on a tile or an env, and neither Destructive
		// nor findRename sees it, so a webhook used to be able to do it
		// unattended.
		if plan.Destructive() || findRename(plan) != nil || hasMove(plan) {
			return false, nil
		}
		if !a.allAuto(ctx, stack, resolved, plan) {
			return false, nil
		}
	}
	if err := a.applyDomainRes(ctx, stack, resolved.Domains, takeDomainRes(plan)); err != nil {
		return false, err
	}
	finishMiddlewares, err := a.applyMiddlewares(ctx, stack, resolved, plan)
	if err != nil {
		return false, err
	}
	if ren := takeRename(plan); ren != nil {
		if err := a.RenameStack(ctx, stack, resolved.Stack, plan); err != nil {
			return false, err
		}
		// The stack pointer carries the new slug now; auto-domain claims in the
		// walk below must generate under it, or the very next plan re-moves them.
		opts.StackSlug = stack.Slug
	}
	// Vars and backup rows are what kept the plan non-empty, and nothing more:
	// applyVars and applyBackups run inside execute unconditionally. Left in,
	// execute would read them as tile changes, since an unrecognised Kind
	// falls through to the tile walk and a backup row carries a real tile
	// name, and create a tile called "vars".
	plan.Changes = slices.DeleteFunc(plan.Changes, func(c Change) bool {
		return c.Tile == "vars" || c.Field == "backup"
	})
	if err := a.execute(ctx, stack, resolved, plan, opts); err != nil {
		return false, err
	}
	finishMiddlewares()
	// Envs were held back for a person, so the plan is not done. It stays
	// pending and the whole thing applies again on approval.
	if held {
		return false, nil
	}
	_ = a.Planner.Store.SetConfigPlanStatus(ctx, cp.ID, "applied")
	return true, nil
}

// isDomainRes marks a stack-level domain-resource change: Env "stack" with
// Field "domain", which no tile differ produces.
func isDomainRes(c Change) bool { return c.Env == "stack" && c.Tile == "" && c.Field == "domain" }

// takeDomainRes removes the domain-resource changes from the plan and returns
// them, execute walks changes by env slug, and "stack" is not an env.
func takeDomainRes(p *Plan) []Change {
	var out, kept []Change
	for _, c := range p.Changes {
		if isDomainRes(c) {
			out = append(out, c)
		} else {
			kept = append(kept, c)
		}
	}
	p.Changes = kept
	return out
}

// applyDomainRes creates and deletes the stack's own domain resources to match
// the file. Deletions run last: a rename in the file (drop one host, add
// another) must not free the old row before the new one is in.
func (a Applier) applyDomainRes(ctx context.Context, stack *repo.Stack, want []DomainResConf, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	store := a.Planner.Store
	conf := map[string]DomainResConf{}
	for _, d := range want {
		conf[d.Host] = d
	}
	all, err := store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	own := map[string]repo.DomainResource{}
	for _, r := range all {
		if r.Level == "stack" && r.OwnerID == stack.ID {
			own[r.Host] = r
		}
	}
	var dels []Change
	for _, c := range changes {
		switch c.Kind {
		case "create":
			d := conf[c.New]
			if err := store.CreateDomainResource(ctx, &repo.DomainResource{
				ID: uuid.New().String(), Level: "stack", OwnerID: stack.ID,
				Host: d.Host, IncludeEnvOnDefault: d.IncludeEnvOnDefault,
				ACMEEmail: d.ACMEEmail, Declared: true, CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		case "update":
			host, _, _ := strings.Cut(c.New, " ")
			r, ok := own[host]
			if !ok {
				continue
			}
			// Also the adoption path: a panel row named in the file becomes
			// the file's, and only then can a later apply delete it.
			want := conf[host]
			r.IncludeEnvOnDefault = want.IncludeEnvOnDefault
			r.Declared = true
			if err := store.UpdateDomainResource(ctx, &r); err != nil {
				return err
			}
			// The ACME account lives in traefik's static config, so it goes
			// through the resource service, which restarts traefik once. This
			// path used to apply acme_email on create only, so changing it in
			// a stack file did nothing.
			if r.ACMEEmail != want.ACMEEmail && a.Ops.Resources != nil {
				if err := a.Ops.Resources.SetACME(ctx, r.ID, want.ACMEEmail); err != nil {
					return err
				}
			}
		case "delete":
			dels = append(dels, c)
		}
	}
	for _, c := range dels {
		if r, ok := own[c.Old]; ok && r.Declared {
			if err := store.DeleteDomainResource(ctx, r.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyUIEdits resolves what the panel does with an edit to a file-owned field
// (this stack's file, then the org file's defaults:, then block) and stores
// it on the stack row so handlers read one column instead of the config.
func (a Applier) applyUIEdits(ctx context.Context, stack *repo.Stack, fileValue string) error {
	mode := fileValue
	if mode == "" {
		if org, err := a.Planner.Store.GetOrg(ctx, stack.OrgID); err == nil && org != nil {
			mode = org.UIEditsDefault
		}
	}
	if mode == stack.UIEditsMode {
		return nil
	}
	stack.UIEditsMode = mode
	return a.Planner.Store.UpdateStack(ctx, stack)
}

// isRename marks the stack-rename change: tile-less with Env "stack", which
// no tile differ produces.
// hasMove reports whether the plan carries a moved: entry. Kind "move" is the
// only change a slug rename produces and nothing else emits it.
func hasMove(p *Plan) bool {
	for _, c := range p.Changes {
		if c.Kind == "move" {
			return true
		}
	}
	return false
}

func isRename(c Change) bool {
	return c.Kind == "update" && c.Env == "stack" && c.Tile == "" && c.Field == "slug"
}

// findRename returns the plan's stack-rename change, nil when there is none.
func findRename(p *Plan) *Change {
	for i := range p.Changes {
		if isRename(p.Changes[i]) {
			return &p.Changes[i]
		}
	}
	return nil
}

// takeRename removes the rename change from the plan and returns it,
// execute walks changes by env slug, and "stack" is not an env.
func takeRename(p *Plan) *Change {
	c := findRename(p)
	if c == nil {
		return nil
	}
	ch := *c
	out := p.Changes[:0]
	for _, cc := range p.Changes {
		if !isRename(cc) {
			out = append(out, cc)
		}
	}
	p.Changes = out
	return &ch
}

// RenameStack moves the stack to the slug its file wants. Container and
// network names derive from the slug (envnet), so every tile's containers are
// stopped under the old name, the row is renamed, and running tiles are
// redeployed under the new one. Data survives: db and volume names are
// tile-id keyed. Tiles the plan deletes (and envs it tears down) stay
// stopped; tiles the plan also updates may deploy twice, correctness over
// thrift, a proxy-only update would otherwise leave its service down.
//
// Exported because the API's PATCH /stacks/{id} needs exactly this: it used to
// write the new slug and stop there, so every running tile kept serving under
// its old service name while the panel resolved the new one, and the runtime
// swallows a stop against a service that is not there (runtime.ScaleService,
// errNoService). p may be an empty Plan: it only names the tiles and envs the
// rename should leave stopped because the same apply is deleting them.
// RenameStackNow is RenameStack with no plan: nothing is being deleted in the
// same breath, so nothing is held down. It is what service.StackRenamer asks
// for — the service layer cannot name a *Plan, since stackconf imports it and
// not the other way round.
func (a Applier) RenameStackNow(ctx context.Context, stack *repo.Stack, name string) error {
	return a.RenameStack(ctx, stack, name, &Plan{})
}

func (a Applier) RenameStack(ctx context.Context, stack *repo.Stack, name string, p *Plan) error {
	store := a.Planner.Store
	deletedEnv, deletedTile := map[string]bool{}, map[string]bool{}
	for _, c := range p.Changes {
		switch c.Kind {
		case "delete-env":
			deletedEnv[c.Env] = true
		case "delete":
			deletedTile[c.Env+"/"+c.Tile] = true
		}
	}
	envs, err := store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return err
	}
	var restart []*repo.Tile
	for i := range envs {
		env := &envs[i]
		tiles, err := store.ListTilesByEnv(ctx, env.ID)
		if err != nil {
			return err
		}
		for j := range tiles {
			t := &tiles[j]
			a.stopContainers(ctx, t)
			if deletedEnv[env.Slug] || deletedTile[env.Slug+"/"+t.Slug] {
				continue
			}
			restart = append(restart, t)
		}
	}
	// No network to clean up: since the swarm move the overlay comes from a
	// pool and its name carries no slug, so a rename leaves it alone. Only
	// container names move, which is what the stop/restart above is for.
	stack.Name, stack.Slug = name, repo.Slugify(name)
	if err := store.UpdateStack(ctx, stack); err != nil {
		return err
	}
	for _, t := range restart {
		a.restartTile(ctx, t)
	}
	return nil
}

// RestartTile is restartTile for the org file's own move applier, which tears
// down shared instances the same way and needs them back up the same way.
func (a Applier) RestartTile(ctx context.Context, t *repo.Tile) { a.restartTile(ctx, t) }

// restartTile brings one tile back up after something its names derive from
// changed: the stack slug, an env slug, or its own slug under a moved: entry.
// Swarm cannot rename a service, so every such change is a teardown, and
// without this the tile simply stays down.
//
// The selection is the substance of it. Errored managed instances heal on
// redeploy (mirrors updateTile); crons and functions have no keep-alive
// container to bring back; anything that was not running stays as it was.
// Best-effort: a redeploy that fails is a warning, the row is already renamed
// and the next deploy picks it up.
func (a Applier) restartTile(ctx context.Context, t *repo.Tile) {
	if t.IsManaged() {
		if t.Status != "running" && t.Status != "error" {
			return
		}
		if a.Instances == nil {
			return
		}
		// The service writes the status either way, which is the half four of
		// the six hand-copied "deploy then status" blocks each got wrong.
		warn("rename redeploy db", t, a.Instances.Deploy(ctx, t))
		return
	}
	if t.Kind != "service" || t.Status != "running" {
		return
	}
	a.deploy(ctx, t, "rename redeploy")
}

// stopContainers removes everything docker holds for a tile: its swarm
// service and any labelled container. Routes, provisions and the row are
// untouched, deletion's extras stay in deleteTile.
func (a Applier) stopContainers(ctx context.Context, t *repo.Tile) {
	envnet.TearDown(ctx, a.Ops.Store, a.Ops.Cluster, t)
}

// ApplyResolved reconciles the database to a caller-provided desired config
// instead of loading it from git, the entry point the UI-staging path uses
// (the desired config comes from StateToResolved + staged edits). It re-diffs
// against current state and runs the same gate + execute as ApplyPlan.
// force=true is human approval (allows deletes, ignores per-env policy);
// force=false applies only when nothing is destroyed and every touched env is
// auto, returning (false, nil) otherwise.
func (a Applier) ApplyResolved(ctx context.Context, stack *repo.Stack, resolved *Resolved, opts DiffOpts, force bool) (bool, error) {
	state, err := a.Planner.Snapshot(ctx, stack)
	if err != nil {
		return false, err
	}
	// Here too, and for the same reason: the diff and execute below have to
	// agree on which environment is the default one.
	opts = withFileDefault(resolved, opts)
	plan := Diff(resolved, state, opts)
	plan.Errors = append(plan.Errors, a.Planner.BadRefs(ctx, stack, resolved, opts.OnlyEnv, opts.SkipEnvs)...)
	if err := a.ensureSecrets(ctx, stack, resolved, opts.OnlyEnv, opts.SkipEnvs); err != nil {
		return false, err
	}
	secretErrs, _, gen := a.Planner.SecretIssues(ctx, stack, resolved, opts.OnlyEnv, opts.SkipEnvs)
	plan.Errors = append(plan.Errors, secretErrs...)
	// Startup-order graph: file loads validate this in resolve(), but staged
	// UI edits build Resolved directly and would otherwise skip it.
	for envName, reEnv := range resolved.Envs {
		if opts.OnlyEnv != "" && envName != opts.OnlyEnv {
			continue
		}
		if err := validateDeps(envName, reEnv.Tiles); err != nil {
			plan.Errors = append(plan.Errors, err.Error())
		}
	}
	plan.GenSecrets = gen
	if len(plan.Errors) > 0 {
		return false, fmt.Errorf("plan has errors: %s", strings.Join(plan.Errors, "; "))
	}
	// A pinned tile whose new node group its home node is not in. Applying
	// would deploy it against a volume that is on another machine, so the
	// move has to happen first (docs/plans/31-node-agent-open-questions.md).
	if moves := a.pendingMoves(ctx, plan.Moves); len(moves) > 0 {
		return false, fmt.Errorf("%s hold volumes that have to be moved first", moveNames(moves))
	}
	if plan.Empty() {
		return true, nil
	}
	if !force {
		if plan.Destructive() {
			return false, nil
		}
		if !a.allAuto(ctx, stack, resolved, plan) {
			return false, nil
		}
	}
	if err := a.applyDomainRes(ctx, stack, resolved.Domains, takeDomainRes(plan)); err != nil {
		return false, err
	}
	finishMiddlewares, err := a.applyMiddlewares(ctx, stack, resolved, plan)
	if err != nil {
		return false, err
	}
	if err := a.execute(ctx, stack, resolved, plan, opts); err != nil {
		return false, err
	}
	finishMiddlewares()
	return true, nil
}

// scope recovers a stored plan's branch and env filter.
func (a Applier) scope(ctx context.Context, stack *repo.Stack, cp *repo.ConfigPlan) (branch, onlyEnv string, skip map[string]bool, err error) {
	if cp.EnvSlug != "" {
		env, gerr := a.Planner.Store.GetEnvironmentBySlug(ctx, stack.ID, cp.EnvSlug)
		if gerr != nil {
			return "", "", nil, gerr
		}
		if env != nil && env.ConfigBranch != "" {
			return env.ConfigBranch, env.Slug, nil, nil
		}
		// A rung above the default env reads the bound branch. It may not
		// exist yet: its own row is what creates it.
		branch, err = a.Planner.StackBranch(ctx, stack)
		return branch, cp.EnvSlug, nil, err
	}
	branch, err = a.Planner.StackBranch(ctx, stack)
	if err != nil {
		return "", "", nil, err
	}
	bound, err := a.Planner.BranchBoundEnvs(ctx, stack)
	if err != nil {
		return "", "", nil, err
	}
	skip = map[string]bool{}
	for slug, b := range bound {
		if b != branch {
			skip[slug] = true
		}
	}
	return branch, "", skip, nil
}

// holdManual adds every declared env whose apply policy is not auto to skip,
// and reports whether it held any back. Policy is per env and manual unless
// someone set it, so on a stack nobody has configured this holds everything
// and an unattended push applies nothing.
func (a Applier) holdManual(ctx context.Context, stack *repo.Stack, r *Resolved, skip map[string]bool) bool {
	envs, err := a.Planner.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return false
	}
	bySlug := map[string]*repo.Environment{}
	for i := range envs {
		bySlug[envs[i].Slug] = &envs[i]
	}
	held, auto := false, 0
	for _, slug := range r.EnvOrder {
		if skip[slug] {
			continue
		}
		protected := false
		if re, ok := r.Envs[slug]; ok {
			protected = re.Protected
		}
		if PolicyFor(bySlug[slug], protected) != "auto" {
			skip[slug] = true
			held = true
			continue
		}
		auto++
	}
	// The home env is not on the ladder (EnvOrder excludes it) and has no
	// policy of its own: it holds the shared instances the other envs cut
	// slices from, so it has to apply whenever any of them does. When none of
	// them does, it waits too, rather than creating databases on a push nobody
	// approved.
	if auto == 0 {
		skip[repo.HomeSlug] = true
	}
	return held
}

// allAuto reports whether every env the plan touches resolves to auto policy.
func (a Applier) allAuto(ctx context.Context, stack *repo.Stack, r *Resolved, p *Plan) bool {
	envs, err := a.Planner.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return false
	}
	defaultSlug := ""
	bySlug := map[string]*repo.Environment{}
	for i := range envs {
		if i == 0 {
			defaultSlug = envs[i].Slug
		}
		bySlug[envs[i].Slug] = &envs[i]
	}
	touched := map[string]bool{}
	for _, c := range p.Changes {
		touched[c.Env] = true
	}
	for slug := range touched {
		if slug == "stack" { // rename, domain resources: the default env's call
			slug = defaultSlug
		}
		protected := false
		if re, ok := r.Envs[slug]; ok {
			protected = re.Protected
		}
		if PolicyFor(bySlug[slug], protected) != "auto" {
			return false
		}
	}
	return true
}

// preflightSlices refuses the whole apply when a slice cuts from an instance
// that neither exists nor is created by this same plan. Every from: in the
// plan is resolved up front, before anything is written, so the answer is
// "nothing was applied and here is what is missing" rather than half an
// environment and a retry line that cannot come true.
//
// An instance created by this plan counts as present: the home env's shared
// instances land in the same apply as the slices that cut from them, and the
// walk orders home first (applyEnvOrder).
func (a Applier) preflightSlices(ctx context.Context, stack *repo.Stack, r *Resolved, p *Plan) error {
	created := map[string]bool{}
	for _, c := range p.Changes {
		if c.Kind == "create" && c.Tile != "" {
			created[c.Tile] = true
		}
	}
	var missing []string
	seen := map[string]bool{}
	for _, c := range p.Changes {
		if c.Kind != "create" || c.Tile == "" {
			continue
		}
		from := r.Envs[c.Env].Tiles[c.Tile].From
		if from == "" {
			continue
		}
		segs := strings.Split(from, ".")
		if created[segs[len(segs)-1]] {
			continue
		}
		// The env may not exist yet, its own row is made by this apply. Only
		// the slug is read, so a stand-in carries everything resolveFrom needs.
		if _, err := a.resolveFrom(ctx, stack, &repo.Environment{Slug: c.Env}, from); err != nil {
			if key := c.Env + "/" + c.Tile; !seen[key] {
				seen[key] = true
				missing = append(missing, key+" cuts from \""+from+"\"")
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("nothing was applied. %d %s cut from an instance that does not exist and this plan does not create:\n  %s",
		len(missing), plural(len(missing), "slice", "slices"), strings.Join(missing, "\n  "))
}

// plural picks the word for n. The apply messages are read by a person under
// pressure and "1 slices" reads as a bug in the thing reporting the bug.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// execute walks the plan's changes and reconciles each touched env.
func (a Applier) execute(ctx context.Context, stack *repo.Stack, r *Resolved, p *Plan, opts DiffOpts) error {
	store := a.Planner.Store
	// ApplyPlan seeds this; ApplyResolved (staged UI edits) does not, and a
	// nil map would make refBroken re-queue a tile this apply already did.
	if a.deployed == nil {
		a.deployed = map[string]bool{}
	}
	// A cron ticking mid-apply runs against tiles that are not there yet.
	if a.Jobs != nil {
		a.Jobs.Hold(stack.ID)
		defer a.Jobs.Release(stack.ID)
	}
	envChanges := map[string]map[string]*tileChange{}
	envCreate := map[string]bool{}
	envDelete := map[string]bool{}
	for _, c := range p.Changes {
		switch c.Kind {
		case "create-env":
			envCreate[c.Env] = true
		case "delete-env":
			envDelete[c.Env] = true
		case "move":
			// Handled by applyMoves below, before anything reads a slug.
		case "update-env":
			// colour: synced from the file for every env below
		default:
			if envChanges[c.Env] == nil {
				envChanges[c.Env] = map[string]*tileChange{}
			}
			tc := envChanges[c.Env][c.Tile]
			if tc == nil {
				tc = &tileChange{fields: map[string]bool{}}
				envChanges[c.Env][c.Tile] = tc
			}
			switch c.Kind {
			case "create":
				tc.create = true
			case "delete":
				tc.del = true
			case "update":
				tc.fields[c.Field] = true
			}
		}
	}

	// Renames before anything reads a slug: containers, routes and generated
	// hostnames all hang off it, and the diff above already treated the row as
	// the same object under its new name.
	if err := a.applyMoves(ctx, stack, r, opts); err != nil {
		return err
	}

	now := time.Now().UTC()
	// Env creations first so tiles have somewhere to land. A row that already
	// exists is kept as-is, the PR-env path pre-creates its ephemeral env
	// (Snapshot skips ephemerals, so the diff always reads it as absent).
	for slug := range envCreate {
		if existing, gerr := store.GetEnvironmentBySlug(ctx, stack.ID, slug); gerr == nil && existing != nil {
			continue
		}
		// Adopt is the declarative door onto the same rules the panel, the
		// API and the PR hook create through. Building the row here instead
		// is how this path skipped the reserved-slug and duplicate-name
		// checks all three of those run.
		if _, err := a.Ops.Envs.Adopt(ctx, stack, service.CreateEnv{
			Name:        strings.ToUpper(slug[:1]) + slug[1:],
			Color:       r.Envs[slug].Color,
			ApplyPolicy: r.Envs[slug].ApplyPolicy,
		}); err != nil {
			return err
		}
	}
	// The stack's own rung of the defaults cascade. Declaring nothing leaves
	// the panel's overrides alone; declaring something replaces them, because
	// the file owns what it declares.
	settingsChanged := false
	if !r.Defaults.Empty() && opts.OnlyEnv == "" {
		if want := r.Defaults.SettingsJSON(); stack.Settings != want {
			stack.Settings = want
			settingsChanged = true
			warn("stack defaults", nil, store.UpdateStack(ctx, stack))
		}
	}
	// The file owns four things per env that are not tiles: its rung, its
	// colour, its apply policy, and which tile keys it sets on purpose (the
	// compare panel's intended marks).
	for i, envName := range r.EnvOrder {
		if (opts.OnlyEnv != "" && envName != opts.OnlyEnv) || opts.SkipEnvs[envName] || envDelete[envName] {
			continue
		}
		env, _ := store.GetEnvironmentBySlug(ctx, stack.ID, envName)
		if env == nil {
			continue
		}
		re := r.Envs[envName]
		// A level the file says nothing about is left alone: declaring nothing
		// is not the same as declaring "inherit everything", and wiping the
		// panel's overrides on every apply would be the second reading.
		wantSettings := env.Settings
		if !re.Defaults.Empty() {
			wantSettings = re.Defaults.SettingsJSON()
		}
		// Colour and apply policy follow the same rule as the settings above,
		// and used to not: they were written on every apply whether or not the
		// file said anything, so a colour set on the canvas or a policy set
		// over the API silently reverted on the next plan. Undeclared means
		// unmanaged — which is what ui_edits and the rest of the gate
		// vocabulary already assume everywhere else.
		wantColor, wantPolicy := env.Color, env.ApplyPolicy
		if re.Color != "" {
			wantColor = re.Color
		}
		if re.ApplyPolicy != "" {
			wantPolicy = re.ApplyPolicy
		}
		// Position is not in that set: it is the file's declaration order, not
		// a value anyone can set from a surface, and the default environment's
		// generated hostname is derived from it (service.defaultEnvID).
		if env.Color != wantColor || env.Position != i || env.ApplyPolicy != wantPolicy || env.Settings != wantSettings {
			settingsChanged = settingsChanged || env.Settings != wantSettings
			env.Color = wantColor
			env.Position = i
			env.ApplyPolicy = wantPolicy
			env.Settings = wantSettings
			warn("env settings", nil, a.Ops.Envs.Save(ctx, env))
		}
		warn("declared overrides", nil, a.syncDeclared(ctx, env, re))
	}
	// Protection feeds the rendered routes, which a settings-only change
	// would otherwise leave as they were.
	if settingsChanged {
		warn("proxy resync", nil, a.Ops.PX.Resync(ctx))
	}
	// Second generation pass: the pre-gate pass could not mint for envs that
	// did not exist yet. Idempotent, an existing value is never touched.
	if len(envCreate) > 0 {
		warn("generate secrets", nil, a.ensureSecrets(ctx, stack, r, opts.OnlyEnv, opts.SkipEnvs))
	}
	// Vars before the tiles below: a tile reading ${{ stack.vars.X }} must find
	// the value this apply is putting there, not the one it had last time.
	warn("write vars", nil, a.applyVars(ctx, stack, r, opts.OnlyEnv, opts.SkipEnvs))
	// After the tiles exist (the loop above created them) and before the
	// backup scheduler is reloaded below.
	if err := a.applyBackups(ctx, stack, r, opts.OnlyEnv, opts.SkipEnvs); err != nil {
		warn("write backups", nil, err)
	} else {
		// The scheduler holds the cron entries in memory, so a written row
		// does not fire until it is reloaded.
		a.Sched.ReloadBackups(ctx)
	}

	// One tile's failure does not abandon the rest. An apply is not atomic,
	// it creates containers and databases, which no transaction can roll back,
	// so aborting at the first error just leaves a different partial state, and
	// hides every other problem until the operator has fixed this one and run
	// again. Convergence is what makes that safe: the tiles that did land are
	// real, the next plan sees them, and a re-apply finishes the job.
	var failed []string
	cronTouched := false
	for _, envSlug := range applyEnvOrder(envChanges, r.EnvOrder) {
		tiles := envChanges[envSlug]
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, envSlug)
		if err == nil && env == nil && envSlug == repo.HomeSlug {
			// A stack made before the home existed (or a wiped row). The
			// store owns the home, so put it back rather than fail the apply.
			env, err = a.Ops.Envs.EnsureHome(ctx, stack.ID, now)
		}
		if err != nil || env == nil {
			return fmt.Errorf("env %s: not found", envSlug)
		}
		re := r.Envs[envSlug]
		slugs := make([]string, 0, len(tiles))
		for slug := range tiles {
			slugs = append(slugs, slug)
		}
		order := applyOrder(slugs, re)
		sliceLanded := false
		for _, tileSlug := range order {
			ch := tiles[tileSlug]
			// A replace shows up as delete+create on the same slug.
			if ch.del {
				if err := a.deleteTile(ctx, env, tileSlug); err != nil {
					failed = append(failed, fmt.Sprintf("%s/%s: %v", envSlug, tileSlug, err))
					continue
				}
				cronTouched = true
			}
			// healthy/completed dependencies block here, in the sequential
			// walk, never inside the deploy worker, which is a single FIFO
			// that would deadlock waiting on a build queued behind it. A
			// timeout warns and applies anyway: convergence beats ordering.
			if tc, ok := re.Tiles[tileSlug]; ok && (ch.create || !ch.del) {
				if wErr := a.waitDeps(ctx, env, tc); wErr != nil {
					failed = append(failed, fmt.Sprintf("%s/%s: %v (applied anyway)", envSlug, tileSlug, wErr))
				}
			}
			if ch.create {
				if err := a.createTile(ctx, stack, env, tileSlug, re.Tiles[tileSlug], opts); err != nil {
					failed = append(failed, fmt.Sprintf("%s/%s: %v", envSlug, tileSlug, err))
					continue
				}
				cronTouched = true
				sliceLanded = sliceLanded || re.Tiles[tileSlug].From != ""
				continue
			}
			if ch.del {
				continue
			}
			cron, err := a.updateTile(ctx, stack, env, tileSlug, re.Tiles[tileSlug], ch.fields, opts)
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s/%s: %v", envSlug, tileSlug, err))
				continue
			}
			cronTouched = cronTouched || cron
		}
		// Bindings are written when a consumer is created or updated. A slice
		// landing after its consumers (added to the file later, or retried
		// after a failed apply) would leave them referencing a resource they
		// are not attached to, so bind the whole env again.
		if sliceLanded {
			existing, lerr := store.ListTilesByEnv(ctx, env.ID)
			warn("list tiles for rebind", nil, lerr)
			for i := range existing {
				tc, ok := re.Tiles[existing[i].Slug]
				if !ok {
					continue
				}
				warn("rebind", &existing[i], a.syncBindings(ctx, &existing[i], tc))
				// Rebinding writes the reference into the tile's variables; it
				// does not restart anything, so a tile that failed to deploy
				// because the resource was not there yet stays failed forever.
				// Seen on the rig: staging/cart-api stuck on
				// "no tile or resource named cart-db" long after cart-db
				// existed, while production's copy was fine purely because it
				// deployed later.
				//
				// Only the ones that are actually broken on a missing
				// reference: everything else in the env is either already
				// running the right thing or was queued by the walk above.
				if a.refBroken(ctx, &existing[i]) {
					a.deploy(ctx, &existing[i], "resource landed")
				}
			}
		}
	}

	// Env deletions last, gated upstream (force only).
	for slug := range envDelete {
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, slug)
		if err != nil || env == nil {
			continue
		}
		if err := a.teardownEnv(ctx, stack, env); err != nil {
			return err
		}
		cronTouched = true
	}

	if cronTouched {
		a.Sched.ReloadCron(ctx)
	}
	if len(failed) > 0 {
		// Sorted so the same broken config reports the same way twice. The
		// total is the tiles this walk set out to change, which is the re-diff
		// against live state and so is smaller than the summary on the stored
		// plan row once part of it has landed. No promise that a re-apply
		// fixes anything: it retries, and whether that works depends on what
		// failed.
		sort.Strings(failed)
		return fmt.Errorf("%d of %d %s failed to apply. The rest were applied, so the environment is part way there; read the errors before re-applying:\n  %s",
			len(failed), changedTiles(envChanges), plural(changedTiles(envChanges), "tile", "tiles"), strings.Join(failed, "\n  "))
	}
	return nil
}

// refBroken reports that a tile's last deploy failed on a reference that did
// not resolve. Matched on the error, which is the only record of why: a
// deployment row carries the message the pipeline produced and nothing
// structured about the cause.
//
// Narrow on purpose. A tile whose build is genuinely broken must not be
// redeployed on every apply that happens to land a slice somewhere in its
// environment, which would turn one bad Dockerfile into a rebuild loop.
func (a Applier) refBroken(ctx context.Context, t *repo.Tile) bool {
	if a.deployed[t.ID] {
		return false // this apply already queued it
	}
	ds, err := a.Planner.Store.ListDeploymentsByTile(ctx, t.ID, 1)
	if err != nil || len(ds) == 0 {
		return false
	}
	d := ds[0]
	return d.Status == "error" && strings.Contains(d.Error, "no tile or resource named")
}

// tileChange is one tile's entry in the per-env change index: what happens to
// it, and which fields moved ("" fields = a create or delete marker).
type tileChange struct {
	create, del bool
	fields      map[string]bool
}

// changedTiles counts the tiles a plan touches, for the "N of M failed" line.
// syncDeclared rewrites the intended rows the file declares for one env:
// every key its overlay sets, any value.
func (a Applier) syncDeclared(ctx context.Context, env *repo.Environment, re ResolvedEnv) error {
	store := a.Planner.Store
	if err := store.ClearDeclaredIntended(ctx, env.ID); err != nil {
		return err
	}
	for tile, keys := range re.Declared {
		for k := range keys {
			if err := store.SetIntended(ctx, &repo.Intended{EnvironmentID: env.ID, TileSlug: tile, Key: k}); err != nil {
				return err
			}
		}
	}
	return nil
}

func changedTiles(envChanges map[string]map[string]*tileChange) int {
	n := 0
	for _, tiles := range envChanges {
		n += len(tiles)
	}
	return n
}

// createTile builds a tile row from its config and starts it (db deploy or
// service build) when the side-effect services are wired.
func (a Applier) createTile(ctx context.Context, stack *repo.Stack, env *repo.Environment, slug string, tc TileConf, opts DiffOpts) error {
	if tc.Type == "slice" {
		return a.createSlice(ctx, stack, env, slug, tc)
	}
	store := a.Planner.Store
	now := time.Now().UTC()
	t := &repo.Tile{
		ID:            uuid.New().String(),
		StackID:       stack.ID,
		EnvironmentID: env.ID,
		Name:          slug,
		Slug:          slug,
		Kind:          desiredKind(tc.Type),
		SourceType:    "image",
		WebhookToken:  uuid.New().String(),
		Status:        "idle",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	applyTileConf(t, tc, stack, opts)
	if tc.Type == "volume" {
		if err := a.resolveAttach(ctx, env, t, tc); err != nil {
			return err
		}
	}
	if tc.Type == "managed" {
		if err := managedtiles.NewDB(t); err != nil {
			return err
		}
	}
	if tc.Type == "cron" {
		if err := jobs.ValidateCron(tc.Schedule); err != nil {
			return fmt.Errorf("tile %s: %w", slug, err)
		}
	}
	if (tc.Type == "cron" || tc.Type == "function") && t.TimeoutMinutes == 0 {
		t.TimeoutMinutes = 30
	}
	// The same validator the panel and the API run. This path checked the
	// cron expression and nothing else, so a file could declare a limit, a
	// port list, a storage attachment or a placement the other two surfaces
	// refuse outright — and the refusal arrived at deploy time, as a failed
	// container, with the plan already marked applied.
	if err := a.Ops.Tiles.Validate(ctx, t); err != nil {
		return fmt.Errorf("tile %s: %w", slug, err)
	}
	if err := store.CreateTile(ctx, t); err != nil {
		return err
	}
	if err := a.Ops.Vars.ReplaceTileVars(ctx, t); err != nil {
		return err
	}
	// After the row exists: a binding points at the consumer id.
	if err := a.syncBindings(ctx, t, tc); err != nil {
		return err
	}
	if tc.Type == "managed" {
		managedtiles.PublishConnection(ctx, store, t)
	}
	if err := a.syncDomains(ctx, env, t, tc, opts, 0); err != nil {
		return err
	}
	switch {
	case tc.Type == "managed" && a.Instances != nil:
		warn("deploy db", t, a.Instances.Deploy(ctx, t))
	case (tc.Type == "service" || tc.Type == "cron" || tc.Type == "function") && a.Engine != nil:
		// A cron/function enqueues the same build; the engine stops at the
		// artifact (no keep-alive container) per its run policy.
		a.deploy(ctx, t, "deploy")
	case tc.Type == "volume":
		a.redeployAttached(ctx, t)
	}
	return nil
}

// applyEnvOrder is the order environments are applied in.
//
// Go randomises map iteration, and the home env holds the stack-scoped
// instances every other env's slices are cut from, so ranging over the change
// map directly created a slice before its instance existed on roughly half of
// all applies. It surfaced as "slice cart-db: from \"sharedpg\": no such
// instance visible from <stack>/<env>" with five of six tiles failing, from a
// plan that declared the instance right there in the same apply.
//
// applyOrder already sorts tiles within one env by dependency; this is the
// same guarantee one level up. Home first, then the file's declared ladder,
// then anything left over, sorted so two runs of the same plan behave alike.
func applyEnvOrder(envChanges map[string]map[string]*tileChange, ladder []string) []string {
	out := make([]string, 0, len(envChanges))
	seen := make(map[string]bool, len(envChanges))
	take := func(slug string) {
		if _, ok := envChanges[slug]; ok && !seen[slug] {
			seen[slug] = true
			out = append(out, slug)
		}
	}
	take(repo.HomeSlug)
	for _, slug := range ladder {
		take(slug)
	}
	rest := make([]string, 0, len(envChanges))
	for slug := range envChanges {
		if !seen[slug] {
			rest = append(rest, slug)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// applyOrder sorts one environment's changed tiles by dependency, because a
// single apply creates tiles that need each other:
//
//	db first    : a slice's from: resolves an instance by address, so the
//	               instance has to exist by the time the slice is cut;
//	slices next : services reference their outputs;
//	volumes last: their attach target has to exist first.
//
// Slugs with no TileConf are strict-mode deletes. They land in the middle pass,
// which is where they ran before this ordering existed.
//
// Sorted within each pass so an apply is reproducible, the caller's slugs come
// out of a map, and "which tile failed first" should not vary run to run.
func applyOrder(slugs []string, re ResolvedEnv) []string {
	pass := func(slug string) int {
		tc, ok := re.Tiles[slug]
		switch {
		case ok && tc.Type == "managed":
			return 0
		case ok && tc.Type == "slice":
			return 1
		case ok && tc.Type == "volume":
			return 3
		default:
			return 2
		}
	}
	out := append([]string(nil), slugs...)
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := pass(out[i]), pass(out[j]); a != b {
			return a < b
		}
		return out[i] < out[j]
	})
	// The runnable pass additionally honours depends_on: the deploy queue is
	// one FIFO worker, so topological enqueue order IS start order.
	lo := 0
	for lo < len(out) && pass(out[lo]) < 2 {
		lo++
	}
	hi := lo
	for hi < len(out) && pass(out[hi]) == 2 {
		hi++
	}
	if hi > lo {
		copy(out[lo:hi], topoDeps(out[lo:hi], re))
	}
	return out
}

// waitDeps blocks until the tile's healthy/completed dependencies are
// satisfied or a per-dependency deadline passes. started needs no wait (FIFO
// deploy worker + topological enqueue order). The deadline stretches with the
// dependency's declared start period, mirroring the deploy health gate.
func (a Applier) waitDeps(ctx context.Context, env *repo.Environment, tc TileConf) error {
	for _, line := range tc.DependsOn {
		slug, cond, err := runtime.ParseDep(line)
		if err != nil || cond == "started" {
			continue // shape errors were validation's job
		}
		dep, err := a.Planner.Store.GetTileBySlug(ctx, env.ID, slug)
		if err != nil || dep == nil {
			return fmt.Errorf("depends_on %s: not found in %s", slug, env.Slug)
		}
		deadline := time.Now().Add(120*time.Second + time.Duration(dep.HealthcheckStartPeriodS)*time.Second)
		for {
			dep, err = a.Planner.Store.GetTileBySlug(ctx, env.ID, slug)
			if err != nil || dep == nil {
				return fmt.Errorf("depends_on %s: vanished mid-apply", slug)
			}
			if cond == "healthy" && dep.Status == "running" {
				break
			}
			// any past ok satisfies completed, a function re-run
			// queued by this same apply may pass on its previous run's ok.
			// The case that matters (first apply, init-container) has no
			// history and waits correctly; track run ids if the race bites.
			if cond == "completed" && dep.LastStatus == "ok" {
				break
			}
			// A dependency parked on an unset value is not going to satisfy
			// anything, polling it to the deadline just delays the apply by
			// two minutes per tile. The engine parks this tile on the same
			// name when its own deploy runs (deploy.DepWaiting).
			if name := deploy.WaitingFor(dep.Status); name != "" {
				return fmt.Errorf("depends_on %s: waiting for %s", slug, name)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("depends_on %s: not %s in time (status %s, last run %s)", slug, cond, dep.Status, dep.LastStatus)
			}
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

// resolveAttach maps a volume's attach slug onto the target tile's id.
func (a Applier) resolveAttach(ctx context.Context, env *repo.Environment, t *repo.Tile, tc TileConf) error {
	t.AttachedTileID = ""
	if tc.Attach == "" {
		return nil
	}
	target, err := a.Planner.Store.GetTileBySlug(ctx, env.ID, tc.Attach)
	if err != nil || target == nil {
		return fmt.Errorf("volume %s: attach target %q not found in %s", t.Slug, tc.Attach, env.Slug)
	}
	t.AttachedTileID = target.ID
	return nil
}

// redeployAttached rebuilds the service mounting a changed volume so the new
// bind takes effect.
func (a Applier) redeployAttached(ctx context.Context, vol *repo.Tile) {
	if a.Engine == nil || vol.AttachedTileID == "" {
		return
	}
	if target, err := a.Planner.Store.GetTile(ctx, vol.AttachedTileID); err == nil && target != nil && target.Kind == "service" && !target.IsManaged() {
		a.deploy(ctx, target, "volume-attached redeploy")
	}
}

// updateTile applies the changed fields onto an existing tile and triggers
// the follow-up (proxy rewrite or redeploy).
func (a Applier) updateTile(ctx context.Context, stack *repo.Stack, env *repo.Environment, slug string, tc TileConf, fields map[string]bool, opts DiffOpts) (cron bool, err error) {
	if tc.Type == "slice" {
		return false, a.updateSlice(ctx, stack, env, slug, tc, fields)
	}
	store := a.Planner.Store
	t, err := store.GetTileBySlug(ctx, env.ID, slug)
	if err != nil || t == nil {
		return false, fmt.Errorf("tile %s: not found in %s", slug, env.Slug)
	}
	oldPort := t.ContainerPort
	// Held before the write: moving a volume from one service to another has
	// to rebuild both, and this path only ever rebuilt the new one, so the
	// old service kept the bind until something else redeployed it.
	oldTarget := ""
	if t.IsVolume() {
		oldTarget = t.AttachedTileID
	}
	applyTileConf(t, tc, stack, opts)
	if tc.Type == "volume" {
		if err := a.resolveAttach(ctx, env, t, tc); err != nil {
			return false, err
		}
	}
	t.UpdatedAt = time.Now().UTC()
	// Same validator as create, and as both surfaces: an edit that would be
	// refused from the panel must be refused from the file.
	if err := a.Ops.Tiles.Validate(ctx, t); err != nil {
		return false, fmt.Errorf("tile %s: %w", slug, err)
	}
	if err := store.UpdateTile(ctx, t.ID, t.TileConfig); err != nil {
		return false, err
	}
	// The file owns this tile's variables: one dropped from the config has to
	// stop resolving, not linger as drift the next plan can't see.
	if err := a.Ops.Vars.ReplaceTileVars(ctx, t); err != nil {
		return false, err
	}
	if err := a.syncBindings(ctx, t, tc); err != nil {
		return false, err
	}
	if t.IsManaged() {
		// ReplaceTileVars just dropped everything the file doesn't declare,
		// including the tile's own published connection details, republish them.
		managedtiles.PublishConnection(ctx, store, t)
	}
	if tc.Type == "volume" {
		a.redeployAttached(ctx, t)
		if oldTarget != "" && oldTarget != t.AttachedTileID {
			a.redeployAttached(ctx, &repo.Tile{AttachedTileID: oldTarget})
		}
		return false, nil
	}
	if err := a.syncDomains(ctx, env, t, tc, opts, oldPort); err != nil {
		return false, err
	}
	cron = t.Kind == "cron" && (fields["schedule"] || fields["command"] || fields["source"] || fields["timeout_minutes"])

	// A db's container carries its port mapping and its resource limits, so a
	// changed row means nothing until it is recreated. Scope is the exception:
	// it only governs who may provision, and no container knows about it.
	// This branch stays here rather than moving into the tile service because
	// the managed-instance lifecycle is its own concept (point 5).
	if t.IsManaged() {
		// "error" redeploys as well as "running": an errored instance is a
		// failure, often this apply's own, since a rejected setting takes the
		// container down, and the next apply is the natural place to heal it.
		// A deliberately "stopped" instance is left alone. On a config-managed
		// stack the file arguably owns "should this be running" outright; that
		// is a bigger semantic change than healing a failure, so it is not
		// made here.
		if a.Instances != nil {
			// A redeploy removes the old container before starting the new one,
			// so a rejected setting (a cpu limit above the host's core count,
			// say) leaves the instance down; the service marks it errored.
			// Reported, not just logged: an apply that says "applied" while the
			// database is down is the failure this whole path exists to avoid.
			if derr := a.Instances.AfterWrite(ctx, t, service.Changed(fields)); derr != nil {
				return false, fmt.Errorf("redeploy: %w", derr)
			}
		}
		return cron, nil
	}
	// Everything else asks the tile service which side effects this change
	// earns, so the panel, the API and a config apply answer from one rule
	// set — but performs them here, because the applier records what it
	// deployed and a deploy queued from inside the service would be missing
	// from that report.
	changed := service.Changed(fields)
	switch {
	case t.Kind == "service":
		// The route is rewritten either way. It used to be rewritten *only*
		// on a proxy-only change, and the deploy engine never writes it
		// itself (1.11) — so an apply that moved a domain and a limit
		// together left the route stale until something else rewrote it.
		warn("rewrite proxy route", t, a.Ops.PX.SyncTile(ctx, t))
		if !changed.ProxyOnly() {
			a.deploy(ctx, t, "deploy")
		}
	case t.Kind == "cron", t.Kind == "function":
		// Only source changes need a rebuild; schedule, command and timeouts
		// are read off the row at each run.
		if changed.NeedsBuild() {
			a.deploy(ctx, t, "build")
		}
	}
	return cron, nil
}

// deleteTile stops a tile's containers, removes its route, orphans any shared-db
// provisions it consumed, and drops the row. Mirrors the app Delete handler so
// UI staged deletes and config-managed git deletes tear down identically.
func (a Applier) deleteTile(ctx context.Context, env *repo.Environment, slug string) error {
	store := a.Planner.Store
	t, err := store.GetTileBySlug(ctx, env.ID, slug)
	if err != nil || t == nil {
		// Not a tile row, a slice answers to the same slug namespace.
		if handled, serr := a.removeSlice(ctx, env, slug); handled || serr != nil {
			return serr
		}
		return nil // already gone
	}
	if t.IsManaged() && a.Instances != nil {
		// An instance is a provider: TileService.TearDown orphans what a tile
		// consumed, which for an instance is nothing. Its own slices, its
		// shared network and the pool entry behind it are this path's, and
		// only this path had none of them. force: the file no longer declares
		// it, which is the decision the held-slices refusal exists to ask for.
		return a.Instances.TearDown(ctx, t, true)
	}
	// The service owns the order and the cascade; this path used to do four of
	// the five steps and skip re-registering the schedule tables the delete
	// cascaded, so a removed cron kept ticking until restart.
	return a.Ops.Tiles.TearDown(ctx, t)
}

// CopyTile writes tc onto an existing tile in env and deploys it: the compare
// panel's "Copy from <reference>". fields names what changed, in the update
// path's terms ("env", "image"). UI-managed stacks only; the caller gates.
func (a Applier) CopyTile(ctx context.Context, stack *repo.Stack, env *repo.Environment, slug string, tc TileConf, fields map[string]bool) error {
	_, err := a.updateTile(ctx, stack, env, slug, tc, fields, DiffOpts{})
	return err
}

// teardownEnv removes an env and everything in it. Falls back to row deletes
// when the runtime services aren't wired (tests).
func (a Applier) teardownEnv(ctx context.Context, stack *repo.Stack, env *repo.Environment) error {
	if a.Ops.RT != nil && a.Ops.PX != nil {
		return a.Ops.Teardown(ctx, stack, env)
	}
	return a.Ops.Envs.Remove(ctx, env.ID)
}

// syncDomains reconciles a service tile's domains to its config. Auto env
// subdomains (managed by stackr) are left alone.
// oldPort is the tile's container port before this apply overwrote it, so a
// config port: change can be carried onto the domain rows that tracked it.
func (a Applier) syncDomains(ctx context.Context, env *repo.Environment, t *repo.Tile, tc TileConf, opts DiffOpts, oldPort int) error {
	if tc.Type != "service" {
		return nil
	}
	store := a.Planner.Store
	cur, err := store.ListDomainsByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	byKey := map[string]repo.Domain{}
	for _, d := range cur {
		byKey[domainKey(d.Host, d.Path, d.Rule)] = d
	}
	seen := map[string]bool{}
	changed := false
	// Path is stored raw ("" and "/" are equivalent to the proxy), creation
	// mirrors diffDomains' keying exactly so applied plans go clean.
	for i, dc := range tc.Domains {
		host, herr := claimHost(dc, env.Slug, t.Slug, opts)
		if herr != nil {
			return herr
		}
		key := domainKey(host, dc.Path, dc.Rule)
		seen[key] = true
		mws := strings.Join(dc.Middlewares, "\n")
		if d, ok := byKey[key]; ok {
			// Traefik routes off Domain.ContainerPort, not Tile.ContainerPort, so a
			// config port: change has to land here too or the proxy keeps routing
			// the old port. Nothing marks a row as config- vs API-created, so the
			// heuristic is "it matched the tile's old port" = it tracked the tile;
			// a domain's own port: wins outright (domainPort).
			newPort := domainPort(dc, tc, oldPort, d.ContainerPort)
			if d.HTTPS != dc.HTTPSOn() || d.ForceHTTPS != dc.ForceHTTPSOn() || d.RedirectTo != dc.RedirectTo || newPort != d.ContainerPort || d.Auto != dc.Auto || d.Position != i ||
				d.Middlewares != mws || d.Priority != dc.Priority {
				d.HTTPS = dc.HTTPSOn()
				d.ForceHTTPS = dc.ForceHTTPSOn()
				d.RedirectTo = dc.RedirectTo
				d.ContainerPort = newPort
				d.Auto = dc.Auto
				d.Position = i
				d.Middlewares = mws
				d.Priority = dc.Priority
				// No partial-update op exists; recreate preserves the id-free bits.
				if err := store.DeleteDomain(ctx, d.ID); err != nil {
					return err
				}
				if err := store.CreateDomain(ctx, &d); err != nil {
					return err
				}
				changed = true
			}
			continue
		}
		// The same resolution the panel and the API use, refusal included.
		// The plan already refused this (checkDomainRules), so reaching it
		// here means the file changed under an approved plan.
		port, perr := service.DomainPort(dc.Port, tc.Port, dc.RedirectTo)
		if perr != nil {
			return fmt.Errorf("tile %s: domain %s: %w", t.Slug, host, perr)
		}
		d := &repo.Domain{
			ID:            uuid.New().String(),
			TileID:        t.ID,
			Host:          host,
			Path:          dc.Path,
			ContainerPort: port,
			HTTPS:         dc.HTTPSOn(),
			ForceHTTPS:    dc.ForceHTTPSOn(),
			RedirectTo:    dc.RedirectTo,
			Auto:          dc.Auto,
			Position:      i,
			Middlewares:   mws,
			Priority:      dc.Priority,
			Rule:          dc.Rule,
			CreatedAt:     time.Now().UTC(),
		}
		if err := store.CreateDomain(ctx, d); err != nil {
			return err
		}
		changed = true
	}
	for key, d := range byKey {
		if seen[key] {
			continue
		}
		if err := store.DeleteDomain(ctx, d.ID); err != nil {
			return err
		}
		changed = true
	}
	if changed && t.Kind == "service" {
		warn("rewrite proxy route", t, a.Ops.PX.SyncTile(ctx, t))
	}
	return nil
}

// applyTileConf maps a TileConf onto a tile row, the write-side mirror of
// diffTile; the two must agree or applied plans never go clean.
func applyTileConf(t *repo.Tile, tc TileConf, stack *repo.Stack, opts DiffOpts) {
	switch tc.Type {
	case "volume":
		t.Kind = "volume"
		t.MountPath = tc.Path
		t.VolumeName = tc.VolumeName
		t.MaxSizeMB = tc.MaxSizeMB
		// attach slug resolves to a tile id in the Applier (needs env context)
	case "managed":
		t.Engine = tc.Engine
		t.ExternalPort = tc.ExternalPort
		t.ShmSizeMB = tc.ShmSizeMB
		t.Replicas = max(tc.Replicas, 1)
		t.NodeGroup = tc.NodeGroup
		// image: is the per-instance override (version pin / wire-compatible
		// build); absent reverts to the engine default rather than sticking.
		if tc.Image != "" {
			t.ImageRef = tc.Image
		} else if eng, ok := managedtiles.Engines[tc.Engine]; ok {
			t.ImageRef = eng.DefaultImage
		}
		t.UpdatePolicy = defStr(tc.UpdatePolicy, "off")
		// ScopeID is derived, never written in the file: "stack" means this
		// stack and "org" means the stack's org, so a copied config block
		// cannot point an instance at someone else's tenant.
		switch tc.Scope {
		case "stack":
			t.ScopeKind, t.ScopeID = "stack", stack.ID
		case "org":
			t.ScopeKind, t.ScopeID = "org", stack.OrgID
		default:
			t.ScopeKind, t.ScopeID = "env", ""
		}
	case "cron":
		t.Kind = "cron"
		applySource(t, tc, stack, opts)
		t.Cron = tc.Schedule
		t.Command = tc.Command
		t.AllowOverlap = tc.AllowOverlap
		t.TimeoutMinutes = defInt(tc.TimeoutMinutes, 30) // absent = default, mirrors diffTile
	case "function":
		t.Kind = "function"
		applySource(t, tc, stack, opts)
		t.Command = tc.Command
		t.RunOnDeploy = tc.RunOnDeploy
		t.AllowOverlap = tc.AllowOverlap
		t.TimeoutMinutes = defInt(tc.TimeoutMinutes, 30)
	case "service":
		t.Kind = "service"
		applySource(t, tc, stack, opts)
		t.ContainerPort = tc.Port
		t.HealthcheckCmd = tc.Healthcheck
		t.SecHeaders = tc.SecurityHeaders
		t.Volumes = strings.Join(tc.Volumes, "\n")
		t.WatchPaths = strings.Join(tc.WatchPaths, "\n")
		t.BuildArgs = tc.BuildArgs
		t.PublishedPorts = tc.PublishedPorts
		t.TraefikOverride = tc.TraefikOverride
		t.BasicAuthUser = tc.BasicAuthUser
		t.BasicAuthPassword = tc.BasicAuthPassword
		t.Command = tc.Command
		t.User = tc.User
		t.ShmSizeMB = tc.ShmSizeMB
		t.Replicas = max(tc.Replicas, 1)
		t.NodeGroup = tc.NodeGroup
		t.Privileged = tc.Privileged
		t.Devices = strings.Join(tc.Devices, "\n")
		t.RestartPolicy, _ = runtime.NormalizeRestart(tc.Restart) // validated by Validate
		t.HealthcheckIntervalS = tc.HealthInterval
		t.HealthcheckTimeoutS = tc.HealthTimeout
		t.HealthcheckRetries = tc.HealthRetries
		t.HealthcheckStartPeriodS = tc.HealthStartPeriod
		t.Files = strings.Join(tc.Files, "\n")
		t.Storage = strings.Join(tc.Storage, "\n")
	}
	// Startup ordering + image-watch/CI knobs apply to every runnable kind.
	switch tc.Type {
	case "service", "cron", "function":
		t.DependsOn = strings.Join(tc.DependsOn, "\n")
		t.UpdatePolicy = defStr(tc.UpdatePolicy, "off")
		t.WaitForCI = tc.WaitForCI
	}
	// nil limits clears, mirroring diffTile, the file is the whole truth.
	t.CPULimit, t.MemLimitMB = 0, 0
	if tc.Limits != nil {
		t.CPULimit = tc.Limits.CPU
		t.MemLimitMB = tc.Limits.MemoryMB
	}
	if tc.Env != nil {
		t.Env = EnvLines(tc.Env)
	}
}

// ensureSecrets mints `default: generated` secrets that have no value yet:
// one per env (env_versions, the default) or one stack-wide row. Idempotent,
// a value at any layer wins and is never touched, so later applies never
// rotate, and changing length: never rewrites an existing value. Rotation
// stays a deliberate panel/CLI act.
func (a Applier) ensureSecrets(ctx context.Context, stack *repo.Stack, r *Resolved, onlyEnv string, skip map[string]bool) error {
	store := a.Planner.Store
	set, err := a.Planner.setVarNames(ctx, stack)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, envName)
		if err != nil || env == nil {
			continue // not created yet; the in-execute pass covers it
		}
		envSet := map[string]bool{}
		if vars, err := store.ListVariables(ctx, repo.OwnerEnv, env.ID); err == nil {
			for _, v := range vars {
				envSet[v.Name] = true
			}
		}
		for name, sc := range r.Envs[envName].Secrets {
			if sc.Default != "generated" || set[name] {
				continue
			}
			v := &repo.Variable{Name: name, Secret: true,
				Value:     secrets.Generate(sc.GenLength(), sc.IncludeNumbers, sc.IncludeSymbols),
				CreatedAt: now, UpdatedAt: now}
			if sc.PerEnv() {
				if envSet[name] {
					continue
				}
				v.OwnerKind, v.OwnerID = repo.OwnerEnv, env.ID
			} else {
				v.OwnerKind, v.OwnerID = repo.OwnerStack, stack.ID
				set[name] = true // one stack-wide mint, not one per env walked
			}
			if err := a.Ops.Vars.Upsert(ctx, v); err != nil {
				return err
			}
			// Minting a secret is a write of a credential, and this path
			// recorded nothing: a generated value appeared with no audit row
			// saying where it came from. The apply is the actor.
			audit.Record(ctx, store, "config:"+stack.Slug, audit.Set, v.OwnerKind, v.OwnerID, name)
			// And a tile parked waiting for this very name is released, which
			// this path also skipped — the value it was waiting for now exists.
			if v.OwnerKind == repo.OwnerEnv {
				deploy.ClearWaitingEnv(ctx, store, v.OwnerID, name)
			} else {
				deploy.ClearWaiting(ctx, store, name, v.OwnerID)
			}
		}
	}
	return nil
}

// applyVars writes the file's vars: onto the stack and env owner rows. Plain
// values only; a name already stored as a secret is left alone, the plan
// already errored on it and overwriting would drop a credential into a row the
// file claims is public.
//
// Nothing is deleted here: see planVars for why an undeclared name is not
// assumed to be the file's to remove.
func (a Applier) applyVars(ctx context.Context, stack *repo.Stack, r *Resolved, onlyEnv string, skip map[string]bool) error {
	store := a.Planner.Store
	now := time.Now().UTC()
	write := func(ownerKind, ownerID string, vars map[string]string, clear func(name string)) error {
		if len(vars) == 0 {
			return nil
		}
		secret := map[string]bool{}
		if cur, err := store.ListVariables(ctx, ownerKind, ownerID); err == nil {
			for _, v := range cur {
				secret[v.Name] = v.Secret
			}
		}
		for name, val := range vars {
			if secret[name] {
				continue
			}
			if err := a.Ops.Vars.Upsert(ctx, &repo.Variable{OwnerKind: ownerKind, OwnerID: ownerID,
				Name: name, Value: val, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			audit.Record(ctx, store, "config:"+stack.Slug, audit.Set, ownerKind, ownerID, name)
			clear(name)
		}
		return nil
	}
	if onlyEnv == "" {
		if err := write(repo.OwnerStack, stack.ID, r.Vars, func(name string) {
			deploy.ClearWaiting(ctx, store, name, stack.ID)
		}); err != nil {
			return err
		}
	}
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, envName)
		if err != nil || env == nil {
			continue
		}
		if err := write(repo.OwnerEnv, env.ID, r.Envs[envName].Vars, func(name string) {
			deploy.ClearWaitingEnv(ctx, store, env.ID, name)
		}); err != nil {
			return err
		}
	}
	return nil
}

// applySource maps a runnable tile's source block onto the row, shared by
// every run policy.
func applySource(t *repo.Tile, tc TileConf, stack *repo.Stack, opts DiffOpts) {
	switch {
	case tc.Image != "":
		t.SourceType = "image"
		t.ImageRef = tc.Image
		// An image tile takes git coordinates only to ship files: from the
		// repo. Set unconditionally: a git tile turned image must lose its old
		// repo, or the next files: entry clones it.
		t.GitURL, t.GitBranch, t.ConnectorID = tc.GitURL, tc.Branch, ""
		if tc.GitURL != "" {
			t.ConnectorID = firstNonEmpty(tc.Connector, stack.ConfigConnectorID)
		}
	default:
		t.SourceType = "git"
		if u := firstNonEmpty(tc.GitURL, opts.GitURL); u != "" {
			t.GitURL = u
		}
		t.ConnectorID = firstNonEmpty(tc.Connector, stack.ConfigConnectorID)
		branch := tc.Branch
		if branch == "" {
			branch = opts.DefaultBranch
		}
		t.GitBranch = branch
		t.BuildContext = defStr(buildContext(tc), ".")
		t.DockerfilePath = defStr(dockerfile(tc), "Dockerfile")
	}
}

// ParsePlan decodes a stored plan's JSON blob.
func ParsePlan(cp *repo.ConfigPlan) *Plan {
	var p Plan
	_ = json.Unmarshal([]byte(cp.Plan), &p)
	return &p
}

// pendingMoves drops the move blocks the operator has already dealt with.
//
// The planner records a block whenever the file asks for a group the tile's
// stored node_group does not match, and deliberately cannot tell whether the
// tile's home node already carries that label: it has no swarm connection and
// must not grow one. But node_group is written by an apply, so nothing ever
// cleared the block and a group pin could never be applied at all, not even
// after the Move the plan asked for had finished.
//
// The applier does have a runtime, so it settles it here with the same
// predicate the deploy uses: placement.For answers cleanly when the tile's
// home node is in the wanted group and errors when it is not.
func (a Applier) pendingMoves(ctx context.Context, ms []MoveBlock) []MoveBlock {
	if a.Ops.RT == nil {
		return ms
	}
	out := ms[:0]
	for _, m := range ms {
		t, err := a.Planner.Store.GetTile(ctx, m.TileID)
		if err != nil || t == nil {
			out = append(out, m)
			continue
		}
		if placement.InGroup(ctx, a.Planner.Store, a.Ops.RT, t, m.ToGroup) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// moveNames lists the tiles standing between a plan and its apply.
func moveNames(ms []MoveBlock) string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Env+"/"+m.Tile)
	}
	return strings.Join(out, ", ")
}
