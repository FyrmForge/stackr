# service/release.go, service/tilediff.go

Source: `service/tilediff.go`, `service/release.go` (+ `service/tilediff_test.go`, mined for facts, code not copied)
Commit: c2423f0
Taken: the field → side-effect mapping — which config keys moved decides whether a write earns a rebuild, a redeploy, a route rewrite, a cron reload or nothing
Cut: all of `release.go` (queue keying, promote-by-commit, the built-commit walk), the `repo.Tile` parameter type, the `infra/runtime` call
Cuts belong to: the new release/promote job (REWRITE.md "Promote and releases"), `leaf/tile`'s own tile struct, and a leaf-local restart-policy fold

## Kept code — target `service/internal/leaf/tile`

```go
// Changed is the set of config keys a write moved, in the config vocabulary
// ("security_headers", "source", "schedule"). It is what decides which side
// effects a write earns.
type Changed map[string]bool

// Any reports whether anything at all moved.
func (c Changed) Any() bool { return len(c) > 0 }

// ProxyOnly reports whether everything that moved lives in the route rather
// than in the container spec. Those apply by rewriting the route; anything
// else needs the container rebuilt.
//
// Nothing changed is not proxy-only: it returns true vacuously on an empty
// set, so the caller checks Any first.
//
// The default arm is fail-safe on purpose: a key nobody classified earns a
// redeploy, never a silent no-op.
func (c Changed) ProxyOnly() bool {
	for f := range c {
		switch {
		case f == "security_headers", f == "basic_auth_user",
			f == "basic_auth_password", f == "traefik_override":
		case strings.HasPrefix(f, "domain "):
			// A config apply names each domain it added, removed or changed
			// individually ("domain +api.example.com"). Route-only too: a host
			// moving does not change the container.
		default:
			return false
		}
	}
	return true
}

// NeedsBuild reports whether a changed field feeds the built artifact.
func (c Changed) NeedsBuild() bool {
	for _, f := range []string{"source", "source_type", "image", "git_url", "connector",
		"branch", "build_context", "dockerfile", "build_args"} {
		if c[f] {
			return true
		}
	}
	return false
}

// NeedsDBRedeploy reports whether a changed field is one a managed instance's
// container embodies. Restarting a database is disruptive enough that it
// should follow from a field that actually needs it.
//
// node_group and replicas are placement, which lives in the service spec
// exactly like the limits do. Leaving them out meant a group pin applied to a
// managed instance wrote the row and touched nothing: the plan read "applied"
// with the database still on the node it was supposed to have left.
func (c Changed) NeedsDBRedeploy() bool {
	for _, f := range []string{"external_port", "cpu_limit", "memory_mb", "limits",
		"env", "image", "shm_size_mb", "node_group", "replicas"} {
		if c[f] {
			return true
		}
	}
	return false
}

// NeedsCronReload reports whether the scheduler's view of this tile moved. A
// cron reads its schedule, command and timeouts off the row at each run, so a
// schedule edit re-registers the table without rebuilding the image.
func (c Changed) NeedsCronReload() bool {
	return c["schedule"] || c["command"] || c["source"] || c["image"] ||
		c["git_url"] || c["timeout_minutes"] || c["allow_overlap"]
}

// canonPolicy and canonRestart fold a field's spellings-of-the-same-thing
// together. Both enums have an empty spelling meaning the default; comparing
// raw reported a change the user never made and redeployed on a save that
// touched nothing.
func canonPolicy(v string) string {
	if v == "" {
		return "off"
	}
	return v
}

// extract: dropped the infra/runtime.NormalizeRestart call, belongs in
// leaf/tile as a local fold. Contract: "" folds to "always"; an unparseable
// value is returned unchanged (Validate refuses it elsewhere).
func canonRestart(v string) string { ... }

// Diff names what moved between the stored tile and the edited one, in the
// same vocabulary the config differ uses.
//
// extract: dropped the *repo.Tile parameter type, belongs in leaf/tile's own
// tile struct — the differ reads fields only, never the store.
//
// Both sides must be loaded-then-edited tiles, not freshly built structs:
// slug and runtime state are outside what a write touches and would otherwise
// read as changes.
func Diff(old, cur *Tile) Changed {
	c := Changed{}
	if old == nil || cur == nil {
		return c
	}
	mark := func(name string, same bool) {
		if !same {
			c[name] = true
		}
	}
	// One mark per row of the spec table below — plain field equality, except
	// for these four. Route-only keys go last, named exactly as ProxyOnly
	// expects.
	mark("restart", canonRestart(old.RestartPolicy) == canonRestart(cur.RestartPolicy))
	mark("update_policy", canonPolicy(old.UpdatePolicy) == canonPolicy(cur.UpdatePolicy))
	mark("limits", old.CPULimit == cur.CPULimit && old.MemLimitMB == cur.MemLimitMB)
	mark("healthcheck", old.HealthcheckCmd == cur.HealthcheckCmd &&
		old.HealthcheckIntervalS == cur.HealthcheckIntervalS &&
		old.HealthcheckTimeoutS == cur.HealthcheckTimeoutS &&
		old.HealthcheckRetries == cur.HealthcheckRetries &&
		old.HealthcheckStartPeriodS == cur.HealthcheckStartPeriodS)
	return c
}
```

```go
// extract: dropped all of release.go — ApplyQueue (Apply keyed by plan id,
// Promote keyed by env), ReleaseService.Target's first-rung-is-not-promoted-to
// rule, PromoteReq{Commit,PlanID,Force}, and built(). built() trips two
// layering filters at once: a store walk over every env/tile/deployment, and a
// status decision (Status == deploystate.Done). All of it belongs in the new
// release/promote job — releases are per-stack numbered snapshots now, so
// "has this commit been built" is a release lookup, not a deployment scan.
```

## Spec: field → side effect

Side effects, cheapest first: **route** = rewrite the proxy route only;
**cron** = re-register the schedule table; **redeploy** = recreate the
container from the image on disk; **build** = rebuild the artifact, then
redeploy.

| Config key | Tile field(s) | Side effect |
|---|---|---|
| `security_headers` | `SecHeaders` | route |
| `basic_auth_user` | `BasicAuthUser` | route |
| `basic_auth_password` | `BasicAuthPassword` | route |
| `traefik_override` | `TraefikOverride` | route |
| `domain …` (prefix) | domains, one key per host added/removed/changed | route |
| `source` | `SourceType` | build + cron |
| `source_type` | (config differ only) | build |
| `image` | `ImageRef` | build + cron + managed redeploy |
| `git_url` | `GitURL` | build + cron |
| `branch` | `GitBranch` | build |
| `connector` | `ConnectorID` | build |
| `dockerfile` | `DockerfilePath` | build |
| `build_context` | `BuildContext` | build |
| `build_args` | `BuildArgs` | build |
| `schedule` | `Cron` | cron |
| `command` | `Command` | cron + redeploy |
| `timeout_minutes` | `TimeoutMinutes` | cron |
| `allow_overlap` | `AllowOverlap` | cron |
| `external_port` | `ExternalPort` | redeploy + managed redeploy |
| `limits` | `CPULimit` **and** `MemLimitMB`, folded | redeploy + managed redeploy |
| `cpu_limit`, `memory_mb` | (config differ only, unfolded) | managed redeploy |
| `env` | (config differ only; the file owns a managed instance's variables) | managed redeploy |
| `shm_size_mb` | `ShmSizeMB` | redeploy + managed redeploy |
| `node_group` | `NodeGroup` | redeploy + managed redeploy |
| `replicas` | `Replicas` | redeploy + managed redeploy |
| `port` | `ContainerPort` | redeploy |
| `published_ports` | `PublishedPorts` | redeploy |
| `user` | `User` | redeploy |
| `restart` | `RestartPolicy`, canonicalised | redeploy |
| `privileged` | `Privileged` | redeploy |
| `devices` | `Devices` | redeploy |
| `depends_on` | `DependsOn` | redeploy |
| `healthcheck` | five `Healthcheck*` fields, folded | redeploy |
| `volumes` | `Volumes` | redeploy |
| `files` | `Files` | redeploy |
| `storage` | `Storage` | redeploy |
| `watch_paths` | `WatchPaths` | redeploy (wart, see notes) |
| `run_on_deploy` | `RunOnDeploy` | redeploy (wart) |
| `update_policy` | `UpdatePolicy`, canonicalised | redeploy (wart) |
| `wait_for_ci` | `WaitForCI` | redeploy (wart) |
| *(nothing moved)* | — | nothing |
| *(unclassified key)* | — | redeploy, by `ProxyOnly`'s default arm |

## Notes for the builder

- **The key is not the field name.** `source`←`SourceType`, `image`←`ImageRef`,
  `branch`←`GitBranch`, `port`←`ContainerPort`, `restart`←`RestartPolicy`,
  `schedule`←`Cron`, `security_headers`←`SecHeaders`. Two keys fold several
  fields: `limits` (cpu + memory) and `healthcheck` (five). Keep the spellings —
  `ProxyOnly`, `NeedsBuild`, `NeedsDBRedeploy` and `NeedsCronReload` match on
  them as strings.
- **Canonicalisation contract** (both helpers are pure string folding):
  `canonPolicy("") == "off"`; `canonRestart("") == "always"`. The old test
  asserted `RestartPolicy` `"always"` vs `""` is *not* a change, which pins the
  second one. An unparseable restart value passes through unchanged.
- **DECIDE: collapse the dual vocabulary.** `source`/`source_type` and
  `limits`/`cpu_limit`+`memory_mb` are both listed only because two differs
  disagreed — this one and the config-plan differ. REWRITE.md kills config
  plans ("No separate apply, no plan rows"), so there should be one vocabulary
  now. Carry the table as the spec, but don't copy a compatibility shim for a
  differ that no longer exists.
- **CHECK: does `NeedsBuild` still have a caller?** REWRITE.md says rebuilds
  come only from an explicit action (push, button, API), and a settings save
  redeploys the image on disk. If so this stops being a write-side-effect
  predicate and becomes "is this key a build input", used by the release
  differ. Keep the mapping either way; verify the caller.
- **`ProxyOnly` is fail-safe and vacuous.** Unclassified key → redeploy, so an
  incomplete table is wrong-but-safe. Empty set → `true`, so callers must check
  `Any()` first or a no-op save rewrites the route.
- **Known wart:** `update_policy`, `wait_for_ci`, `run_on_deploy` and
  `watch_paths` feed only the image watcher or the build path, yet they fall
  through `ProxyOnly`'s default and earn a full container redeploy. Not spec —
  fix it by classifying them, and the fail-safe default still covers the rest.
- **Free checks worth keeping** (from the old test, restated, not copied):
  identical tiles earn nothing; every spec field (port, limits, healthcheck,
  command, replicas, node_group, volumes, image) must fail `ProxyOnly`; all
  four route fields plus a `domain +host` key must pass it; a schedule change
  reloads the cron but must not build.

Size: source 341 lines, extract 230 lines
