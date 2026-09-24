# infra/deploy

Source: `internal/stackrd/infra/deploy/` (engine.go, waiting.go)
Commit: c2423f0
Taken: clone/fetch/checkout at a ref, remote branch listing, the repo-escape path guard, the built-image naming scheme
Cut: the whole deploy engine — queue and dispatch, the pipeline, status mapping, park/release/supersede, registry push and auth, file materialization, spec building, the rollout wait, image pruning
Cuts belong to: `service/internal/flow/deploy`, `service/internal/flow/jobs`, `service/internal/leaf/tile`

## Kept code

```go
// Package git clones and checks out tile repositories. It shells out to the
// git binary. No store handle, no Docker handle, no decisions about a deploy.
package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// allowProtocol stops git from following a redirect or a submodule into a
// local or exotic transport. The URL allowlist at the boundary refuses those
// in the URL itself; this covers what git resolves on its own.
const allowProtocol = "GIT_ALLOW_PROTOCOL=https:ssh"

// Repo is one tile's working clone on disk.
type Repo struct {
	Dir    string // the clone, e.g. <dataDir>/repos/<tile-id>
	URL    string
	Branch string // "" = the remote's default

	// Auth returns extra environment lines (GIT_CONFIG_COUNT/KEY/VALUE…)
	// that authenticate the fetch — a GitHub App installation token, say.
	// Environment, never argv: argv is world-readable in /proc, so a token
	// in the URL leaks to every process on the box.
	//
	// extract: dropped the *repo.Tile argument and the connector lookup
	// behind it, belongs in flow/deploy.
	Auth func(ctx context.Context) []string
}

func (r Repo) env(ctx context.Context) []string {
	env := append(os.Environ(), allowProtocol)
	if r.Auth != nil {
		env = append(env, r.Auth(ctx)...)
	}
	return env
}

// Checkout makes r.Dir a clone of r.URL checked out at ref, and returns the
// resulting commit SHA. ref is a branch or a commit; "" means r.Branch, and
// no branch means the remote's default. The clone tracks r.Branch and ref is
// fetched on top, so a commit that is not on that branch still resolves.
// w receives git's own output (the job log).
func (r Repo) Checkout(ctx context.Context, ref string, w io.Writer) (string, error) {
	if ref == "" {
		ref = r.Branch
	}
	if ref == "" {
		ref = "HEAD"
	}
	// extract: dropped the ValidGitURL guard that stood here, belongs at the
	// boundary that accepts a tile's git_url (leaf/tile validate).
	env := r.env(ctx)
	run := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout = w
		cmd.Stderr = w
		return cmd.Run()
	}
	if _, err := os.Stat(filepath.Join(r.Dir, ".git")); err != nil {
		// Anything there without a .git is a half-written clone from a killed
		// job, and not repairable. Start over.
		_ = os.RemoveAll(r.Dir)
		args := []string{"clone", "--single-branch"}
		if r.Branch != "" {
			args = append(args, "--branch", r.Branch)
		}
		// "--" so a URL beginning with a dash is not read as a flag.
		if err := run("", append(args, "--", r.URL, r.Dir)...); err != nil {
			return "", err
		}
	}
	if err := run(r.Dir, "fetch", "origin", ref); err != nil {
		return "", err
	}
	if err := run(r.Dir, "checkout", "-f", "FETCH_HEAD"); err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "git", "-C", r.Dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Branches lists the remote's branch names. Best effort: nil on any error
// (bad URL, auth, timeout), because it only fills a picker.
func (r Repo) Branches(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "--", r.URL)
	cmd.Env = r.env(ctx)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			branches = append(branches, strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/"))
		}
	}
	return branches
}

// Under resolves a tile-supplied path (build context, Dockerfile, a files:
// entry) against the clone and refuses one that lands outside it. The clone
// sits next to every other tile's clone, deploy keys and rendered files, so a
// context of "../../keys" with a one-line COPY Dockerfile would ship them all
// in a pullable image.
func (r Repo) Under(p string) (string, error) {
	full := filepath.Join(r.Dir, filepath.FromSlash(p))
	rel, err := filepath.Rel(r.Dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes the repo", p)
	}
	return full, nil
}
```

Image naming is not git; the builder parks it where images live (`leaf/image`
is the fit). It is here because it is the other half of this row.

```go
// ImageName is the name of an image built from a commit. repoName is
// stkr/<org>_<stack>_<tile-slug>: the same name for every environment of a
// stack, which is what lets a promote run the image a lower env built.
//
//	stkr/acme_shop_api:9f3c1ab
//
// The tag is the commit, so a rollback or a promote is "run that commit's
// image" and nothing has to be rebuilt to get it.
//
// extract: dropped the store lookup that resolved org/stack/tile into
// repoName, belongs in flow/deploy (it passes the name in).
// extract: dropped the "does this tag already exist locally" check that
// appended "-"+jobID[:8] on a rebuild of the same commit — it is a Docker
// call, belongs in leaf/image. Keep the rule: a second build of one commit
// must not overwrite the image an older release still points at.
func ImageName(repoName, sha, jobID string) string {
	if len(sha) >= 7 {
		return repoName + ":" + sha[:7]
	}
	// No SHA (an image tile, or a fetch that produced none): the job id.
	return repoName + ":" + jobID[:8]
}
```

## Cut, by name and destination

`flow/jobs` — `DeployKind`, `deployJob`, `WithWork` and its `KindOpts`
(30-minute timeout, requeue-on-restart, the supersede callback), `reopen`,
`dispatch`, `failQueued`, the in-memory `queue`/`worker`/`cancels` fallback,
`Cancel`, the per-deploy log file and the `logWriter` that tees it to the
stream hub.

`flow/deploy` — `Enqueue`, `EnqueueRollback`, `EnqueuePromote`,
`EnqueueCurrent`, `enqueueImage`, `CurrentImage`; `Park`, `Release`,
`FailWaiting`, `SupersedeWaiting` (the CI gate itself is explicitly later);
`run` (deployment and tile status mapping, notifications, `OnFinish`);
`pipeline` (which source a trigger picks, pull/push credentials,
`pushToRegistry`, `managedAuth`, `hasImage`, `imageRef`'s store half);
`materializeFiles`, `pruneFiles`; the spec builders `volumeBinds`,
`publishedPorts`, `parseKV`, `splitLines`, `splitCommand`, `serviceCommand`,
`warnUnsupported`; `builder`, `buildkitSpec`, `BuilderFor` (the build-node
choice; the buildx calls themselves come from `infra/runtime`); all of
waiting.go — `WaitingPrefix`, `WaitingFor`, `DepWaiting`, `ClearWaiting`,
`ClearWaitingEnv`, `ClearWaitingOrg` (the tiles `status` column goes away, so
this becomes job state).

`leaf/tile` — `retireContainers`; `pruneImages` (keep-last-5) goes to
`leaf/image`.

Rebuilt, not ported: `awaitService` and `reason` were a reader for Swarm's own
rolling update. The health gate is written fresh in the plan.

## Notes for the builder

- **Auth never touches the URL.** No token is interpolated into the clone URL
  anywhere in the old code; it arrives as `GIT_CONFIG_*` env lines. Keep that
  — argv shows up in `ps` and `/proc`, and a clone URL is echoed into the job
  log that the UI streams.
- **`GIT_ALLOW_PROTOCOL` is load-bearing.** A URL allowlist only checks the
  URL you were given; git follows redirects and submodules on its own. Port
  the old `ValidGitURL` allowlist too, at the boundary that accepts `git_url`.
- **Not shallow.** `--single-branch`, no `--depth`. That is deliberate: a
  promote checks out a commit that may not be on the tile's branch, and a
  shallow clone cannot fetch it. If depth is ever added, `--depth` plus
  `fetch origin <sha>` needs `uploadpack.allowReachableSHA1InWant` on the
  server side.
- **Detached HEAD by design.** `fetch origin <ref>` then
  `checkout -f FETCH_HEAD` resolves a branch and a raw SHA through one path.
  `-f` throws away whatever the last build left in the tree.
- **Clone dirs are long-lived, per tile.** `<dataDir>/repos/<tile-id>`, reused
  across deploys, so the cheap path is a fetch. They are only removed when
  `.git` is missing; nothing garbage-collects them when a tile is deleted —
  that is a gap worth closing in the tile delete flow, not in this package.
  `<dataDir>/{repos,deploy-logs,keys,files}` were created at engine start.
- **Timeouts.** A deploy ran under a 30-minute context and the queue kind used
  the same 30 minutes; `ls-remote` gets its own 10 seconds. `exec.CommandContext`
  kills git on cancel, which is how a cancelled deploy stopped a clone.
- **Missing on purpose:** there is no "read a file at a commit" and no "list
  changed paths between commits" in this package. Nothing here calls
  `git show`, `git cat-file` or `git diff`; config files are read from the
  checked-out tree, through `Under`. Build one only if a caller needs it.
- **The check to port:** `Under` has the only non-obvious logic here. Keep its
  table test — refused: `"../../keys"`, `".."`, `"sub/../../other"`,
  `"../tile2"`; allowed: `"."`, `""`, `"app"`, `"..hidden"`,
  `"/abs/is/rooted/in/repo"`, `"a/../b"`.

## Domain rules seen (spec, not code)

These are what the engine enforced. Rule 9: unlisted domain rules behave as
today, so they need a home in the new flows.

- A volume tile is not deployable at all.
- Queueing any deploy first cancels that tile's parked deploys — newer always
  outranks older, so a gate released late cannot put an old commit back on top.
- Deploys dedupe per tile; a superseded one ends `cancelled`, never silently
  dropped, or its row sits "queued" with a Cancel button and no runner.
- A restart re-runs a deploy the restart interrupted, from the top; a cancel
  or a genuine build failure is never re-run behind the operator's back.
- Trigger picks the source: a rollback and a promote run a named image if it
  exists; a promote whose image is absent builds at that commit.
- A promote needs at least 7 characters of commit.
- A config change on an environment above the bottom rung redeploys the image
  the tile already runs — an apply never ships code nobody promoted.
- A git tile with no repository set fails with an instruction ("add one under
  Settings, or change its source to a Docker image"), not a field name.
- Every build pushes; a failed push fails the deploy, because an image that
  exists only on the builder is not a deployable artifact.
- Registry credentials travel per request, never `docker login`: the daemon
  config is shared by every org on the box.
- A credential is only ever offered to this install's own registry host;
  Docker Hub and third-party hosts pull anonymously.
- Build context and Dockerfile path must both resolve inside the clone.
- A tile is never started with an unresolved `${{ }}` reference.
- A reference to a value nobody has set is not a failure: the tile parks on
  that name, and setting the value releases it to stopped — never an
  automatic deploy, because setting a variable is not a request to ship.
- A tile whose dependency is parked parks on the same name, so one value
  releases the whole chain.
- Releasing is scoped: an env-scoped value frees only that env's tiles, an
  org-scoped one every stack in the org; a variable name is not unique across
  tenants.
- A kind that does not keep a container alive (cron) stops at the build and
  ends idle, not running; its trigger runs the built image.
- A cancelled deploy leaves the tile stopped, not errored — a cancel is not a
  broken tile.
- `files:` ship out of the repo even for an image tile, so such a tile must
  carry git coordinates; each deploy gets a fresh folder, symlinks are never
  copied, and the old folder is removed only after the new one is serving.
- The last 5 successful deployments' images are kept per tile, counted across
  every environment's copy of that tile, and the rest are removed.
- A tile holding a volume runs exactly one replica; one mounter per volume.
- A service tile's command is tokenized into argv, never wrapped in `sh -c`;
  cron keeps `sh -c`. The two lifecycles differ on purpose.
- A malformed published-port line warns in the log and is skipped; it does not
  fail the deploy. Settings the runtime cannot honour are warned about out
  loud rather than silently dropped.
- "The image this tile runs" is the newest deployment that finished
  successfully, skipping any failures on top of it.

Size: source 1560 lines, extract 283 lines
