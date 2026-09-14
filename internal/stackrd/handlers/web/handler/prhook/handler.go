// Package prhook turns GitHub pull_request webhooks into ephemeral
// environments: opened → clone the configured base env and deploy it,
// synchronize → redeploy the PR branch, closed → tear the env down.
package prhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store   repo.Store
	engine  *deploy.Engine
	jobs    *jobs.Service
	ops     envops.Ops
	applier stackconf.Applier
	gh      *githubapp.Client
	// notifier exists only to nudge open canvases after a push re-plans: the
	// plan banner is server-rendered, so a plan landing here is invisible
	// until something tells the page to re-fetch.
	notifier *notify.Notifier
	// work is the durable job runner. An auto-apply goes on it rather than
	// running inline on GitHub's delivery request.
	work *workqueue.Queue
}

func NewHandler(store repo.Store, engine *deploy.Engine, jobsSvc *jobs.Service, ops envops.Ops, applier stackconf.Applier, gh *githubapp.Client, notifier *notify.Notifier) *handler {
	return &handler{store: store, engine: engine, jobs: jobsSvc, ops: ops, applier: applier, gh: gh, notifier: notifier}
}

// WithWork gives the webhook the durable runner. Without it an auto-apply runs
// inline on the delivery request, and GitHub hangs up after about ten seconds,
// which cancelled almost every auto-apply on push.
func (h *handler) WithWork(q *workqueue.Queue) *handler { h.work = q; return h }

func (h *handler) planner() stackconf.Planner { return h.applier.Planner }

type prPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Hook handles POST /hooks/github/:stack. Signature-verified against the
// stack's configured secret; non-pull_request events are acknowledged and
// ignored.
func (h *handler) Hook(c echo.Context) error {
	ctx := c.Request().Context()
	stack, err := h.store.GetStack(ctx, c.Param("stack"))
	if err != nil {
		return err
	}
	if stack == nil {
		return echo.NewHTTPError(http.StatusNotFound, "unknown stack")
	}
	cfg := envops.LoadPRConfig(ctx, h.store, stack.ID)
	if !cfg.Enabled {
		return echo.NewHTTPError(http.StatusNotFound, "pr environments disabled")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20))
	if err != nil {
		return err
	}
	if !validSignature(cfg.Secret, c.Request().Header.Get("X-Hub-Signature-256"), body) {
		return echo.NewHTTPError(http.StatusUnauthorized, "bad signature")
	}
	if c.Request().Header.Get("X-GitHub-Event") != "pull_request" {
		return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
	}
	var p prPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Number == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
	}

	if err := h.dispatch(ctx, stack, cfg, &p); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// HookConnector handles POST /hooks/connectors/:id, a connector's GitHub
// App webhook endpoint. Signature-verified with that connector's webhook
// secret; target stacks are the connector's org's PR-enabled stacks whose
// tiles track the payload repo.
func (h *handler) HookConnector(c echo.Context) error {
	ctx := c.Request().Context()
	cn, err := h.store.GetConnector(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if cn == nil {
		return echo.NewHTTPError(http.StatusNotFound, "unknown connector")
	}
	secret := githubapp.ParseConfig(cn.Config).WebhookSecret
	if secret == "" {
		return echo.NewHTTPError(http.StatusNotFound, "connector not connected")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20))
	if err != nil {
		return err
	}
	if !validSignature(secret, c.Request().Header.Get("X-Hub-Signature-256"), body) {
		return echo.NewHTTPError(http.StatusUnauthorized, "bad signature")
	}
	switch c.Request().Header.Get("X-GitHub-Event") {
	case "pull_request":
		var p prPayload
		if err := json.Unmarshal(body, &p); err != nil || p.Number == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
		}
		stacks, err := h.store.ListStacks(ctx)
		if err != nil {
			return err
		}
		handled, failed := 0, 0
		for i := range stacks {
			stack := &stacks[i]
			if stack.OrgID != cn.OrgID {
				continue
			}
			// Config plan preview on the PR, independent of PR environments.
			if stack.ConfigManaged() && stack.ConfigRepo == p.Repository.FullName {
				h.updatePlanComment(ctx, stack, &p)
			}
			cfg := envops.LoadPRConfig(ctx, h.store, stack.ID)
			if !cfg.Enabled || !h.stackTracksRepo(ctx, stack.ID, &p) {
				continue
			}
			// One stack failing must not starve the rest of the org.
			if err := h.dispatch(ctx, stack, cfg, &p); err != nil {
				c.Logger().Errorf("prhook: stack %s: %v", stack.Slug, err)
				failed++
				continue
			}
			handled++
		}
		status := http.StatusOK
		if failed > 0 {
			status = http.StatusInternalServerError // surfaces in GitHub's delivery log
		}
		return c.JSON(status, map[string]int{"handled": handled, "failed": failed})
	case "push":
		var p pushPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
		}
		// One push drives both paths: a config change deploys the tiles it
		// touched, and the branch build below covers the rest. The shared set
		// is what stops a tile both paths want from queueing two builds of
		// the same commit.
		planned := h.planConfigs(ctx, cn.OrgID, &p)
		deployed := map[string]bool{}
		return c.JSON(http.StatusOK, map[string]int{"deployed": h.autoDeploy(ctx, cn.OrgID, &p, deployed), "planned": planned})
	default:
		return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
	}
}

type pushPayload struct {
	Ref     string `json:"ref"`     // refs/heads/<branch>
	After   string `json:"after"`   // head commit SHA
	Deleted bool   `json:"deleted"` // branch/tag deletion push
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// changedFiles flattens the push's touched paths. Empty when the payload
// carries no commit list (force pushes, very large pushes), callers must
// treat that as "anything could have changed".
func (p *pushPayload) changedFiles() []string {
	var out []string
	for _, c := range p.Commits {
		out = append(out, c.Added...)
		out = append(out, c.Removed...)
		out = append(out, c.Modified...)
	}
	return out
}

// configOnlyPush reports whether every changed file is the stack's config
// file, such pushes re-plan but skip the app rebuild.
// include files still trigger rebuilds; watch-paths is the real fix.
func configOnlyPush(s *repo.Stack, changed []string) bool {
	if !s.ConfigManaged() || len(changed) == 0 {
		return false
	}
	path := s.ConfigPath
	if path == "" {
		path = stackconf.DefaultPath
	}
	for _, f := range changed {
		if f != path {
			return false
		}
	}
	return true
}

// planConfigs re-plans every config-managed stack bound to the pushed
// repo+branch: env-scoped plans for envs bound to this branch, the
// stack-scoped plan when the stack's own branch was pushed. Pending plans
// whose touched envs are all auto-policy (and nothing is destroyed)
// apply immediately.
// deployed is gone from here: the apply is asynchronous now, so nothing it
// queues is known by the time autoDeploy runs. The engine supersedes a waiting
// deploy of the same tile, which is what keeps that from being two builds.
func (h *handler) planConfigs(ctx context.Context, orgID string, p *pushPayload) int {
	pl := h.planner()
	if pl.Store == nil || pl.Src == nil {
		return 0
	}
	branch := strings.TrimPrefix(p.Ref, "refs/heads/")
	if branch == p.Ref || p.Deleted {
		return 0
	}
	stacks, err := h.store.ListStacks(ctx)
	if err != nil {
		return 0
	}
	planned := 0
	// Org config first: a push to the org file re-plans the org. Plans only,
	// org applies are always human-approved, never webhook-driven.
	if org, err := h.store.GetOrg(ctx, orgID); err == nil && org != nil &&
		org.ConfigManaged() && org.ConfigRepo == p.Repository.FullName {
		orgBranch := org.ConfigBranch
		if orgBranch == "" {
			if cn, cerr := h.store.GetConnector(ctx, org.ConfigConnectorID); cerr == nil && cn != nil {
				orgBranch, _ = h.applier.Planner.Src.DefaultBranch(ctx, cn, org.ConfigRepo)
			}
		}
		if orgBranch == branch {
			r := orgconf.Runner{Store: h.store, Src: h.applier.Planner.Src, Stacks: h.applier.Planner, Applier: h.applier}
			if _, err := r.Plan(ctx, org, p.After); err != nil && err != orgconf.ErrNoFile {
				slog.Error("org config plan failed", "org", org.Slug, "error", err)
			} else {
				planned++
			}
		}
	}
	for i := range stacks {
		s := &stacks[i]
		if s.OrgID != orgID || !s.ConfigManaged() || s.ConfigRepo != p.Repository.FullName {
			continue
		}
		envs, err := h.store.ListEnvironmentsByStack(ctx, s.ID)
		if err != nil {
			continue
		}
		for j := range envs {
			if envs[j].Type != "static" || envs[j].ConfigBranch != branch {
				continue
			}
			cp, err := pl.RunEnv(ctx, s, &envs[j], p.After)
			if err != nil {
				slog.Error("config plan failed", "stack", s.Slug, "env", envs[j].Slug, "error", err)
				continue
			}
			planned++
			h.maybeAutoApply(ctx, s, cp)
			h.notifier.Project(s.ID)
		}
		if sb, err := pl.StackBranch(ctx, s); err == nil && sb == branch {
			cps, err := pl.Run(ctx, s, p.After)
			if err != nil && err != stackconf.ErrNoFile {
				slog.Error("config plan failed", "stack", s.Slug, "error", err)
				continue
			}
			if err == nil {
				planned += len(cps)
				// One row per commit now. Auto-apply lands the envs whose
				// policy is auto and leaves the plan pending for the rest
				// (Applier.holdManual).
				for _, cp := range cps {
					h.maybeAutoApply(ctx, s, cp)
				}
				h.notifier.Project(s.ID)
			}
		}
	}
	return planned
}

// maybeAutoApply queues a fresh pending plan. What actually gets applied is
// decided inside the job: the environments whose policy is auto, and nothing
// destroyed (Applier.holdManual, allAuto). Failures land on the plan row.
//
// It no longer takes the per-request "already deployed" set. That set let the
// branch build skip tiles the apply had queued, and the two no longer share a
// request. The engine supersedes a waiting deploy of the same tile, so the
// cost is a superseded row, not a double build.
func (h *handler) maybeAutoApply(ctx context.Context, s *repo.Stack, cp *repo.ConfigPlan) {
	if cp == nil || cp.Status != "pending" {
		return
	}
	// On the queue, never inline. GitHub gives a delivery about ten seconds
	// before it hangs up, and a client disconnect cancels the request context
	// in net/http, so an auto-apply on push was cut off almost every time.
	if h.work == nil {
		return
	}
	if _, err := stackconf.EnqueueApply(ctx, h.work, s, cp, false); err != nil {
		slog.Error("config auto-apply could not be queued", "stack", s.Slug, "plan", cp.ID, "error", err)
	}
}

// autoDeploy redeploys every git service tile in the org's default
// environments that tracks the pushed repo+branch. Envs above the default
// take images by promote, never from a push. PR-env tiles are excluded, the
// pull_request "synchronize" event already covers those.
func (h *handler) autoDeploy(ctx context.Context, orgID string, p *pushPayload, done map[string]bool) int {
	branch := strings.TrimPrefix(p.Ref, "refs/heads/")
	if branch == p.Ref || p.Deleted { // tag/other ref, or branch deletion
		return 0
	}
	stacks, err := h.store.ListStacks(ctx)
	if err != nil {
		return 0
	}
	deployed := 0
	for i := range stacks {
		if stacks[i].OrgID != orgID {
			continue
		}
		// A push that only touches the config file re-plans the stack but
		// must not rebuild the app, nothing in the image changed.
		if configOnlyPush(&stacks[i], p.changedFiles()) {
			continue
		}
		envs, err := h.store.ListEnvironmentsByStack(ctx, stacks[i].ID)
		if err != nil {
			continue
		}
		for _, env := range envs {
			if env.Type != "static" || !isDefaultEnv(envs, &env) {
				continue
			}
			tiles, err := h.store.ListTilesByEnv(ctx, env.ID)
			if err != nil {
				continue
			}
			for j := range tiles {
				t := &tiles[j]
				// Crons rebuild on push exactly like services, same source,
				// same watch rules; only the lifecycle after the build differs.
				if t.SourceType == "git" && (t.Kind == "service" || t.Kind == "cron") && t.GitBranch == branch &&
					sameRepo(t.GitURL, p.Repository.CloneURL, p.Repository.SSHURL, p.Repository.FullName) {
					if !watchMatch(t.WatchPaths, p.changedFiles()) {
						continue // push didn't touch this tile's watched paths
					}
					if done[t.ID] {
						continue // the config apply for this push already queued it
					}
					// wait_for_ci parks the deploy until the commit's checks
					// pass; the CI-gate janitor releases or fails it. Without
					// a connector there is nothing to ask, deploy as usual.
					if t.WaitForCI && t.ConnectorID != "" && p.After != "" {
						if _, err := h.engine.Park(ctx, t, "push", p.After); err == nil {
							deployed++
						}
						continue
					}
					if _, err := h.engine.Enqueue(ctx, t, "push"); err == nil {
						deployed++
					}
				}
			}
		}
	}
	return deployed
}

// isDefaultEnv is the first static env, the bottom rung of the ladder.
// store order, like allAuto; the config's declared order wins once
// both read it from one place.
func isDefaultEnv(envs []repo.Environment, env *repo.Environment) bool {
	for i := range envs {
		if envs[i].Type == "static" {
			return envs[i].ID == env.ID
		}
	}
	return false
}

func (h *handler) dispatch(ctx context.Context, stack *repo.Stack, cfg envops.PRConfig, p *prPayload) error {
	slug := fmt.Sprintf("pr-%d", p.Number)
	switch p.Action {
	case "opened", "reopened":
		return h.openPR(ctx, stack, cfg, slug, p)
	case "synchronize":
		return h.syncPR(ctx, stack, slug)
	case "closed":
		return h.closePR(ctx, stack, slug)
	default:
		return nil
	}
}

// stackTracksRepo reports whether any git tile in any of the stack's
// environments points at the webhook's repository.
func (h *handler) stackTracksRepo(ctx context.Context, stackID string, p *prPayload) bool {
	envs, err := h.store.ListEnvironmentsByStack(ctx, stackID)
	if err != nil {
		return false
	}
	for _, env := range envs {
		tiles, err := h.store.ListTilesByEnv(ctx, env.ID)
		if err != nil {
			continue
		}
		for _, t := range tiles {
			if t.SourceType == "git" && sameRepo(t.GitURL, p.Repository.CloneURL, p.Repository.SSHURL, p.Repository.FullName) {
				return true
			}
		}
	}
	return false
}

func (h *handler) openPR(ctx context.Context, stack *repo.Stack, cfg envops.PRConfig, slug string, p *prPayload) error {
	if env, err := h.store.GetEnvironmentBySlug(ctx, stack.ID, slug); err != nil {
		return err
	} else if env != nil {
		return h.syncPR(ctx, stack, slug) // reopened with env still around
	}
	// A PR env is a template instantiation: base + pr_envs.tiles, built purely
	// from the file through the ordinary apply engine, never a clone of a
	// live env, so panel drift can't leak into previews.
	resolved := h.fileResolved(ctx, stack)
	if resolved == nil || resolved.PRTemplate == nil {
		return nil // no pr_envs: in the file, nothing to build
	}
	pre := resolved.PREnvs
	if pre.Enabled != nil && !*pre.Enabled {
		return nil
	}
	if len(pre.Against) > 0 && !contains(pre.Against, p.PullRequest.Base.Ref) {
		return nil
	}
	// comment:/status: are read off the stored PRConfig by deploy feedback
	// (githubapp/feedback.go), so the file's choice has to be persisted to
	// take effect. The file overwrites the panel toggle, consistent with a
	// config-managed stack, where the file owns the settings.
	// read-through instead if githubapp can reach the config
	// without an import cycle.
	if pre.Comment != nil || pre.Status != nil {
		want := cfg
		if pre.Comment != nil {
			want.NoComment = !*pre.Comment
		}
		if pre.Status != nil {
			want.NoStatus = !*pre.Status
		}
		if want != cfg {
			cfg = want
			_ = envops.SavePRConfig(ctx, h.store, stack.ID, cfg)
		}
	}
	env := &repo.Environment{
		ID:        uuid.New().String(),
		StackID:   stack.ID,
		Name:      strings.ToUpper(slug[:2]) + " " + slug[3:], // "PR 42"
		Slug:      slug,
		Type:      "ephemeral",
		Settings:  "{}",
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.CreateEnvironment(ctx, env); err != nil {
		return err
	}
	// One-env desired state from the template, reconciled by the apply engine:
	// slice provisioning (fresh copies, createSlice keys on env.Type), binding
	// sync, per-env secret generation, auto per-PR hostnames, deploy enqueues.
	// DefaultBranch = the PR head, so every tile inheriting the bound repo's
	// branch builds the PR branch.
	// a tile tracking a different repo also inherits the PR branch
	// when it declares no branch:, split the default per-repo if that bites.
	r := &stackconf.Resolved{
		Stack:    resolved.Stack,
		EnvOrder: []string{slug},
		Envs:     map[string]stackconf.ResolvedEnv{slug: *resolved.PRTemplate},
	}
	opts := h.planner().Opts(ctx, stack, p.PullRequest.Head.Ref, slug, nil)
	if _, err := h.applier.ApplyResolved(ctx, stack, r, opts, true); err != nil {
		return err
	}
	// A PR env has no base env, its board starts from the default (oldest)
	// env's layout so matching slugs land where the team arranged them.
	if envs, err := h.store.ListEnvironmentsByStack(ctx, stack.ID); err == nil && len(envs) > 0 {
		envops.CopyLayout(ctx, h.store, envs[0].ID, env.ID)
	}
	_ = h.jobs.LoadSchedules(ctx)
	return nil
}

// updatePlanComment maintains the PR's sticky comment plan section: a
// preview of what merging this PR into its base branch would change in the
// stack. Closed PRs clear the stored section.
func (h *handler) updatePlanComment(ctx context.Context, stack *repo.Stack, p *prPayload) {
	if h.gh == nil {
		return
	}
	pl := h.planner()
	cn, err := h.store.GetConnector(ctx, stack.ConfigConnectorID)
	if err != nil || cn == nil {
		return
	}
	num := strconv.Itoa(p.Number)
	if p.Action == "closed" {
		_ = h.store.SetSetting(ctx, githubapp.PlanKey(stack.ID, num), "")
		return
	}
	base := p.PullRequest.Base.Ref
	sb, err := pl.StackBranch(ctx, stack)
	if err != nil {
		return
	}
	bound, _ := pl.BranchBoundEnvs(ctx, stack)
	onlyEnv := ""
	var skip map[string]bool
	if base == sb {
		skip = map[string]bool{}
		for slug, b := range bound {
			if b != sb {
				skip[slug] = true
			}
		}
	} else {
		for slug, b := range bound {
			if b == base {
				onlyEnv = slug
				break
			}
		}
		if onlyEnv == "" {
			return // merging this base affects no config scope
		}
	}
	plan, err := pl.Preview(ctx, stack, p.PullRequest.Head.Ref, base, onlyEnv, skip)
	var md string
	switch {
	case err == stackconf.ErrNoFile:
		return // no config on the PR branch, nothing to preview
	case err != nil:
		md = "### stackr plan for `" + base + "`\n\n> ⚠️ config invalid: " + err.Error() + "\n"
	default:
		md = planMarkdown(plan, base)
	}
	_ = h.store.SetSetting(ctx, githubapp.PlanKey(stack.ID, num), md)
	env, _ := h.store.GetEnvironmentBySlug(ctx, stack.ID, "pr-"+num)
	h.gh.RefreshPRComment(ctx, cn, p.Repository.FullName, num, stack.ID, env)
}

// planMarkdown renders a plan as the PR comment section: summary line, a
// diff-fenced change list (GitHub colours +/-), and any blocking errors.
func planMarkdown(plan *stackconf.Plan, base string) string {
	var b strings.Builder
	b.WriteString("### stackr plan, merging into `" + base + "`\n\n")
	b.WriteString("**" + plan.Summary() + "**")
	if plan.Destructive() {
		b.WriteString(" · ⚠️ destructive, approval required")
	}
	b.WriteString("\n")
	if len(plan.Changes) > 0 {
		b.WriteString("\n```diff\n")
		for _, ch := range plan.Changes {
			sign := "~"
			switch ch.Kind {
			case "create", "create-env":
				sign = "+"
			case "delete", "delete-env":
				sign = "-"
			}
			line := sign + " " + ch.Env
			if ch.Tile != "" {
				line += "/" + ch.Tile
			}
			if ch.Field != "" {
				line += " " + ch.Field
			}
			if ch.Old != "" || ch.New != "" {
				line += ": "
				if ch.Old != "" {
					line += ch.Old + " → "
				}
				line += ch.New
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("```\n")
	}
	for _, e := range plan.Errors {
		b.WriteString("\n> ⚠️ " + e + "\n")
	}
	return b.String()
}

// fileResolved fetches and resolves the bound config at the stack's branch
// (nil when unbound, file missing, or fetch fails).
func (h *handler) fileResolved(ctx context.Context, stack *repo.Stack) *stackconf.Resolved {
	pl := h.planner()
	if !stack.ConfigManaged() || pl.Store == nil || pl.Src == nil {
		return nil
	}
	branch, err := pl.StackBranch(ctx, stack)
	if err != nil {
		return nil
	}
	resolved, err := pl.LoadResolved(ctx, stack, branch)
	if err != nil {
		return nil
	}
	return resolved
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (h *handler) syncPR(ctx context.Context, stack *repo.Stack, slug string) error {
	env, err := h.store.GetEnvironmentBySlug(ctx, stack.ID, slug)
	if err != nil {
		return err
	}
	if env == nil {
		return nil // PR was filtered at open (against/enabled), nothing to sync
	}
	tiles, err := h.store.ListTilesByEnv(ctx, env.ID)
	if err != nil {
		return err
	}
	for i := range tiles {
		t := tiles[i]
		// Crons rebuild on a PR push exactly like services, same source, same
		// artifact; only the lifecycle after the build differs. autoDeploy skips
		// PR-env tiles entirely on the strength of this loop covering them.
		if t.SourceType == "git" && (t.Kind == "service" || t.Kind == "cron") {
			_, _ = h.engine.Enqueue(ctx, &t, "webhook")
		}
	}
	return nil
}

func (h *handler) closePR(ctx context.Context, stack *repo.Stack, slug string) error {
	env, err := h.store.GetEnvironmentBySlug(ctx, stack.ID, slug)
	if err != nil {
		return err
	}
	if env == nil {
		return nil // already gone
	}
	if err := h.ops.Teardown(ctx, stack, env); err != nil {
		return err
	}
	_ = h.jobs.LoadSchedules(ctx)
	return nil
}

// sameRepo loosely matches a tile's git URL against the webhook repo:
// normalized https/ssh forms or the bare full name.
func sameRepo(tileURL string, candidates ...string) bool {
	n := normalizeRepo(tileURL)
	for _, c := range candidates {
		if c != "" && normalizeRepo(c) == n {
			return true
		}
	}
	return false
}

func normalizeRepo(u string) string {
	u = strings.TrimSpace(strings.ToLower(u))
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "ssh://")
	u = strings.TrimPrefix(u, "git@")
	u = strings.Replace(u, ":", "/", 1) // git@host:org/repo -> host/org/repo
	return strings.Trim(u, "/")
}

func validSignature(secret, header string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.TrimPrefix(header, "sha256=")))
}
