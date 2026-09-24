# Step 2: infra wrappers (Docker first)

Read first: `docs/rewrite/PROGRESS.md`, then REWRITE.md "Rules from the first
commit" rule 5, "Infrastructure", "Build order" row 2. Extracts:
`docs/rewrite/extracts/{runtime,deploy,imagewatch-infra,backup-infra,
githubapp}.md`.

Build order row 2 names the Docker wrapper. The git, registry, S3 and VIP
wrappers are the same kind of thing (rule-free, resolved input, no domain
word in them) and are built here too so step 3 is only services. Every
package here lives under `service/internal/` and holds no rule: if a
function needs to know what a tile or an environment is, it belongs in a
leaf.

## Tasks

1. **`internal/docker`, containers.** Implement the `service.Docker`
   interface from step 1 with the Docker SDK, from the `runtime` extract's
   plain-container path: create from a resolved `ContainerSpec` (image by
   digest, env, command, mounts, networks with aliases, labels, restart
   policy, healthcheck, resource limits, host networking + cap add for the
   proxy/stackrd cases), start, stop with timeout, remove, inspect (returns
   health state, running, restart count, IPs per network in one call),
   restart, exec (stdin/stdout streams, exit code), logs (follow stream with
   timestamps, tail), list by label.
   Done when: a build-tagged integration test against the local daemon
   creates, inspects, execs, logs and removes a container.

2. **`internal/docker`, networks and volumes.** Create/remove/connect/
   disconnect with alias, list by label; volume create/remove/inspect/list
   by label. Both idempotent (exists = fine).
   Done when: integration test covers create twice, connect with alias,
   remove.

3. **`internal/docker`, images.** Pull by `repo@digest` with an optional
   registry credential, resolve `repo:tag` → digest for this arch, image
   list/remove by label, build via buildx from a context dir + Dockerfile +
   build args, streaming the build output to an `io.Writer` (the job log)
   and returning the image id. Prune helper for images no release references
   (caller passes the keep list).
   Done when: integration test builds a hello Dockerfile and pulls a small
   public image by digest.

4. **`internal/registry`.** From the `imagewatch-infra` extract: registry
   client with pull-credential auth, `HEAD` manifest → digest for a tag
   (resolved to this box's arch), list tags. One call per unique image, the
   caller dedupes. Errors typed, never "no update".
   Done when: unit test against a recorded response; integration test
   against Docker Hub for one public image.

5. **`internal/git`.** From the `deploy` extract: clone at a commit into a
   dir, checkout, read a file at a commit, list changed paths between two
   commits (for `watch_paths`), using a token URL the connector hands in.
   Image naming helper for built images.
   Done when: tests run against a local bare repo made in `t.TempDir()`.

6. **`internal/vip`.** iptables DNAT rules in the host network namespace:
   `Set(vipIP, replicaIPs)` rewrites the rule set for one VIP,
   `Remove(vipIP)`, `Rebuild(all)` from a list, using a stackr-owned chain
   so nothing else's rules are touched. No knowledge of tiles: IPs in, rules
   out.
   Done when: unit tests over the generated rule text; integration test
   behind a build tag needing `NET_ADMIN`.

7. **`internal/s3`** and the local destination. From the `backup-infra`
   extract: one `Destination` interface (put stream, get stream, list,
   delete), two implementations: local dir under the data dir, S3. Streams
   only, no temp files bigger than the scratch-space preflight allows.
   Done when: the local implementation round-trips in a test; S3 against a
   minio container behind a build tag.

8. **`internal/githubapp`.** From the `githubapp` extract: app JWT,
   installation token, webhook signature verify + push-event parse, clone
   URL with token. No CI/feedback code.
   Done when: signature and token tests pass with fixtures.

9. **`internal/proxy`, the Caddy admin client.** `Push(config)` to the admin
   API, `Get()`, reachable over the proxy container's admin address. Config
   building (routes, upstreams, basic auth, extras) is a leaf concern and is
   step 3; this package only ships JSON.
   Done when: a test pushes and reads back a config against a Caddy
   container behind a build tag.

10. **Done gate.** `make build`, `make lint`, `make test` (unit) pass;
    `make test-integration` (build tag) passes on a box with Docker;
    PROGRESS.md ticked; stacked PR.

## Not in this step

No leaf, no flow, no rollout logic, no health gate: those read these
wrappers and live in step 3.
