package project

import (
	"context"
	"fmt"
	"github.com/FyrmForge/stackr/internal/deploystate"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/gitlog"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The commit log in the environments panel on the stack canvas: the last ten
// commits on the bound branch and a chip per environment on the commit it
// runs, plans, deploys or failed on.

const logLen = 10

// logChip is one environment's mark on a commit.
type logChip struct {
	Env    repo.Environment
	Color  string
	State  string // runs | plan | building | deploying | failed | plan-error
	Href   string // runs: the env canvas; plan: its row on the plans page; failed/deploying: the deployment
	Behind int    // commits behind the branch head (runs only)
	// Done/Total: tiles finished out of those queued for the commit
	// (building and deploying only). Building carries no env: an image is
	// built once and promoted around, so the chip is the stack's, not a rung's.
	Done, Total int
}

// envRun says where an env stands against the branch head: how many
// commits behind the one it runs is, or that the branch does not contain
// it. found is false for an env with no done deployment.
func (l commitLog) envRun(envID string) (behind int, offBranch, found bool) {
	for _, row := range l.OffBranch {
		for _, ch := range row.Chips {
			if ch.State == "runs" && ch.Env.ID == envID {
				return 0, true, true
			}
		}
	}
	for _, rows := range [][]logRow{l.Rows, l.Older} {
		for _, row := range rows {
			for _, ch := range row.Chips {
				if ch.State == "runs" && ch.Env.ID == envID {
					return ch.Behind, false, true
				}
			}
		}
	}
	return 0, false, false
}

// sameCommit is true when every deployed env runs the branch head.
func (l commitLog) sameCommit(envs []repo.Environment) bool {
	n := 0
	for _, e := range envs {
		behind, off, found := l.envRun(e.ID)
		if !found {
			continue
		}
		if behind > 0 || off {
			return false
		}
		n++
	}
	return n > 1
}

// logRow is one commit and the chips on it. Behind is how many commits
// separate it from the branch head, known for the rows in the window (their
// position) and for older rows the log asked the connector about.
type logRow struct {
	Commit     gitlog.Commit
	Chips      []logChip
	Behind     int
	HaveBehind bool
}

// commitLog is what the block renders.
type commitLog struct {
	Rows      []logRow
	Older     []logRow // chips on commits older than the ten, still on the branch
	OffBranch []logRow // chips on commits the branch does not contain (force push, hotfix)
	Branch    string
	Repo      string // owner/name on GitHub, the clone dir's base name locally
	Source    string // github | local | ""
	FetchedAt string // age of the list
	// Err is why the fetch failed; Rows then hold the last good list and
	// StaleFor says how old it is.
	Err      string
	StaleFor string
	// ConnectorsURL is where to look when GitHub is not answering.
	ConnectorsURL string
	// Envs is the static ladder in order, the first one being the rung that
	// builds on push, with Colors keyed by env id and Runs holding the commit
	// each one runs. Built are the commits some environment has a finished
	// deployment for. All four come out of the pass that builds the chips,
	// the releases page needs them and the deployment reads are not free.
	//
	// Built only sees the last few deploys per tile, so a commit nothing has
	// touched in a while reads as not built.
	Envs   []repo.Environment
	Colors map[string]string
	Runs   map[string]string
	Built  map[string]bool
}

type logCacheEntry struct {
	commits []gitlog.Commit
	branch  string
	source  string
	dir     string    // the local clone, source local
	at      time.Time // when commits were last fetched successfully
	checked time.Time // when we last tried
	err     string
	// older is what we learned about commits outside the list, keyed by sha.
	// Behind counts are relative to the head, so the map resets when it moves.
	older map[string]olderInfo
}

// olderInfo is one lookup. known is false when the lookup failed: the row
// stays in Older with no behind count, and checked says when to try again.
type olderInfo struct {
	commit   gitlog.Commit
	behind   int
	onBranch bool
	known    bool
	checked  time.Time
}

// olderMu guards the older maps. fetchCommits hands the same map to every
// request until the head moves, and two canvases rendering at once wrote it
// together.
var olderMu sync.Mutex

// logCache is one list per stack, refreshed at most once a minute: the page
// refreshes on every project event and GitHub rate-limits.
var logCache sync.Map

// fetchCommits returns the stack's recent commits, cached for a minute.
func (h *handler) fetchCommits(ctx context.Context, p *repo.Stack) logCacheEntry {
	if v, ok := logCache.Load(p.ID); ok {
		e := v.(logCacheEntry)
		if time.Since(e.checked) < time.Minute {
			return e
		}
	}
	var prev logCacheEntry
	if v, ok := logCache.Load(p.ID); ok {
		prev = v.(logCacheEntry)
	}
	commits, branch, source, dir, err := h.loadCommits(ctx, p)
	e := logCacheEntry{commits: commits, branch: branch, source: source, dir: dir, at: time.Now(), checked: time.Now(), older: prev.older}
	if len(commits) > 0 && len(prev.commits) > 0 && commits[0].SHA != prev.commits[0].SHA {
		e.older = nil
	}
	if err != nil {
		// keep the last good list under a banner
		e = prev
		e.checked = time.Now()
		e.err = err.Error()
		if e.branch == "" {
			e.branch = branch
		}
	}
	logCache.Store(p.ID, e)
	return e
}

// olderCommit is what the log shows for a commit outside its list: the commit
// itself, how far behind the head it is, and whether the branch has it at
// all. One call per sha per head, remembered in the cache entry. A failed
// lookup is remembered too, for the same minute fetchCommits waits, so a
// GitHub that is down or rate limiting costs one render a minute, not every
// one.
func (h *handler) olderCommit(ctx context.Context, p *repo.Stack, e *logCacheEntry, sha string) olderInfo {
	olderMu.Lock()
	if o, ok := e.older[sha]; ok && (o.known || time.Since(o.checked) < time.Minute) {
		olderMu.Unlock()
		return o
	}
	if e.older == nil {
		e.older = map[string]olderInfo{}
	}
	olderMu.Unlock()
	o := olderInfo{commit: gitlog.Commit{SHA: sha}, onBranch: true, checked: time.Now()}
	remember := func(o olderInfo) olderInfo {
		olderMu.Lock()
		e.older[sha] = o
		olderMu.Unlock()
		logCache.Store(p.ID, *e)
		return o
	}
	head := e.branch
	if len(e.commits) > 0 {
		head = e.commits[0].SHA
	}
	var err error
	switch e.source {
	case "github":
		cn, cerr := h.store.GetConnector(ctx, p.ConfigConnectorID)
		if cerr != nil || cn == nil {
			return remember(o)
		}
		if cm, cerr := h.gh.Commit(ctx, cn, p.ConfigRepo, sha); cerr == nil {
			o.commit = cm
		} else {
			err = cerr
		}
		if err == nil {
			o.behind, o.onBranch, err = h.gh.Behind(ctx, cn, p.ConfigRepo, sha, head)
		}
	case "local":
		if cm, cerr := gitlog.One(ctx, e.dir, sha); cerr == nil {
			o.commit = cm
		} else {
			err = cerr
		}
		if err == nil {
			o.behind, o.onBranch, err = gitlog.Behind(ctx, e.dir, sha, head)
		}
	default:
		return o
	}
	if err != nil {
		// unknown stays "older" with no behind count; tried again in a minute
		o.behind, o.onBranch = 0, true
		if ctx.Err() != nil {
			// The page's budget ran out, not GitHub: not remembered, or one
			// slow render would hide every viewer's counts for a minute.
			return o
		}
		return remember(o)
	}
	o.known = true
	return remember(o)
}

// loadCommits reads from GitHub through the stack's connector, or from the
// local clone of the reference env's first git tile when there is none.
func (h *handler) loadCommits(ctx context.Context, p *repo.Stack) (commits []gitlog.Commit, branch, source, dir string, err error) {
	if p.ConfigManaged() && h.gh != nil {
		cn, cerr := h.store.GetConnector(ctx, p.ConfigConnectorID)
		if cerr != nil || cn == nil {
			return nil, "", "", "", fmt.Errorf("config connector: not found")
		}
		branch, _ = h.applier.Planner.StackBranch(ctx, p)
		commits, err = h.gh.Commits(ctx, cn, p.ConfigRepo, branch, logLen)
		return commits, branch, "github", "", err
	}
	if h.engine() == nil {
		return nil, "", "", "", nil
	}
	envs, _ := h.envs.ListForStack(ctx, p.ID)
	for _, e := range envs {
		if e.Type == "ephemeral" {
			continue
		}
		tiles, _ := h.tiles.ListForEnv(ctx, e.ID)
		for i := range tiles {
			t := &tiles[i]
			if t.SourceType != "git" {
				continue
			}
			dir = h.engine().RepoDir(t)
			commits, err = gitlog.Local(ctx, dir, "origin/"+t.GitBranch, logLen)
			if err != nil {
				commits, err = gitlog.Local(ctx, dir, "", logLen)
			}
			return commits, t.GitBranch, "local", dir, err
		}
		break
	}
	return nil, "", "", "", nil
}

// commitLog gathers the block: commits plus what every env runs, plans,
// deploys or failed on.
func (h *handler) commitLog(ctx context.Context, p *repo.Stack) commitLog {
	e := h.fetchCommits(ctx, p)
	log := commitLog{Branch: e.branch, Source: e.source, Err: e.err,
		Runs: map[string]string{}, Built: map[string]bool{}}
	if e.err != "" && !e.at.IsZero() {
		log.StaleFor = ago(e.at)
	}
	if !e.at.IsZero() {
		log.FetchedAt = ago(e.at)
	}
	switch e.source {
	case "github":
		log.Repo = p.ConfigRepo
	case "local":
		log.Repo = filepath.Base(e.dir)
	}
	h.fillOrg(ctx, p)
	log.ConnectorsURL = "/orgs/" + p.OrgSlug + "/settings/connectors"

	index := map[string]int{}
	for i, c := range e.commits {
		index[c.SHA] = i
		log.Rows = append(log.Rows, logRow{Commit: c})
	}
	older := map[string]*logRow{}
	place := func(sha string, chip logChip) {
		if sha == "" {
			return
		}
		if i, ok := index[sha]; ok {
			chip.Behind = i
			log.Rows[i].Chips = append(log.Rows[i].Chips, chip)
			return
		}
		row := older[sha]
		if row == nil {
			row = &logRow{Commit: gitlog.Commit{SHA: sha}}
			older[sha] = row
		}
		row.Chips = append(row.Chips, chip)
	}
	envHref := func(env repo.Environment) string { return stackURL(p) + "/" + env.Slug }

	envs, _ := h.envs.ListForStack(ctx, p.ID)
	colors := h.envColorsByID(ctx, p, envs)
	log.Colors = colors
	plans, _ := h.store.ListConfigPlans(ctx, p.ID, 30)
	building := map[string]bool{} // commits that already carry a building chip
	first := true
	for _, env := range envs {
		if env.Type == "ephemeral" {
			continue
		}
		chip := logChip{Env: env, Color: colors[env.ID]}
		runs, live, failed, prog, built := h.envDeployments(ctx, env.ID)
		log.Envs = append(log.Envs, env)
		for sha := range built {
			log.Built[sha] = true
		}
		if runs != nil {
			log.Runs[env.ID] = runs.CommitSHA
		}
		// One chip per env per commit: two tiles deploying the same push
		// would otherwise put a done chip next to a spinner on one row.
		liveSHA := ""
		if live != nil {
			liveSHA = live.CommitSHA
			if liveSHA == "" && len(e.commits) > 0 {
				liveSHA = e.commits[0].SHA // queued: not built yet, so it is the head
			}
		}
		if runs != nil && runs.CommitSHA != liveSHA {
			c := chip
			c.State, c.Href = "runs", envHref(env)
			place(runs.CommitSHA, c)
		}
		if live != nil {
			c := chip
			c.State, c.Href = "deploying", "/deployments/"+live.ID
			c.Done, c.Total = prog.Done, prog.Total
			if prog.Building {
				// The image is what is being made; every rung gets it
				// afterwards. One stack-level chip, no env colour.
				c.State, c.Env, c.Color = "building", repo.Environment{}, ""
			}
			// only the bottom rung builds, so a second env building
			// the same commit is a "keep one chip" case, not a sum.
			if c.State != "building" || !building[liveSHA] {
				building[liveSHA] = building[liveSHA] || c.State == "building"
				place(liveSHA, c)
			}
		}
		if failed != nil {
			c := chip
			c.State, c.Href = "failed", "/deployments/"+failed.ID
			place(failed.CommitSHA, c)
		}
		// the newest undecided plan row for this env: the default env's is
		// the stack-scoped row (EnvSlug "")
		for _, cp := range plans {
			mine := cp.EnvSlug == env.Slug || (first && cp.EnvSlug == "")
			if !mine {
				continue
			}
			c := chip
			c.Href = "/projects/" + p.ID + "/config/plans/" + cp.ID
			switch cp.Status {
			case "pending":
				c.State = "plan"
			case "error":
				c.State = "plan-error"
			default: // clean rows: the env's own ↑n chip already says it is behind
				continue
			}
			place(cp.CommitSHA, c)
			break
		}
		first = false
	}
	// Commits outside the list: fill in the commit, the distance to the head,
	// and whether the branch has it at all. One page budget for all of them.
	octx, cancel := components.PageCtx(ctx)
	defer cancel()
	for sha, row := range older {
		o := h.olderCommit(octx, p, &e, sha)
		row.Commit = o.commit
		row.Behind, row.HaveBehind = o.behind, o.known && o.onBranch
		for i := range row.Chips {
			if row.Chips[i].State == "runs" {
				row.Chips[i].Behind = o.behind
			}
		}
		if o.onBranch {
			log.Older = append(log.Older, *row)
		} else {
			log.OffBranch = append(log.OffBranch, *row)
		}
	}
	newest := func(rows []logRow) func(i, j int) bool {
		return func(i, j int) bool { return rows[i].Commit.When.After(rows[j].Commit.When) }
	}
	sort.Slice(log.Older, newest(log.Older))
	sort.Slice(log.OffBranch, newest(log.OffBranch))
	return log
}

// envDeployments is what one env's git tiles say: the newest finished
// deployment (what it runs), one in flight, and the newest one if it failed.
// envProgress is where an env's in-flight push stands: how many git tiles
// have a deploy for it, how many finished, and whether one is still building
// its image.
type envProgress struct {
	Done, Total int
	Building    bool
}

// built collects every commit this env has a finished deployment for, which
// is what the releases page calls an artifact: the image exists, because
// something ran it.
func (h *handler) envDeployments(ctx context.Context, envID string) (runs, live, failed *repo.Deployment, prog envProgress, built map[string]bool) {
	built = map[string]bool{}
	tiles, _ := h.tiles.ListForEnv(ctx, envID)
	for i := range tiles {
		if tiles[i].SourceType != "git" {
			continue
		}
		deps, _ := h.store.ListDeploymentsByTile(ctx, tiles[i].ID, 5)
		// The tile's newest deploy is its part of the batch in flight.
		if len(deps) > 0 {
			switch deps[0].Status {
			case deploystate.Done:
				prog.Done++
				prog.Total++
			default:
				if deploystate.IsLive(deps[0].Status) {
					prog.Total++
					if tiles[i].Status == "building" {
						prog.Building = true
					}
				}
			}
		}
		for j := range deps {
			d := &deps[j]
			switch d.Status {
			case deploystate.Done:
				if d.CommitSHA != "" {
					built[d.CommitSHA] = true
				}
				if d.CommitSHA != "" && (runs == nil || d.CreatedAt.After(runs.CreatedAt)) {
					runs = d
				}
			case deploystate.Error:
				// only the tile's newest attempt counts as "failed"
				if j == 0 && d.CommitSHA != "" && (failed == nil || d.CreatedAt.After(failed.CreatedAt)) {
					failed = d
				}
			default:
				if deploystate.IsLive(d.Status) && (live == nil || d.CreatedAt.After(live.CreatedAt)) {
					live = d
				}
			}
		}
	}
	return runs, live, failed, prog, built
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
