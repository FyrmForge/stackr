# Step 0: scaffold and docs

Read first: `docs/rewrite/PROGRESS.md`, then REWRITE.md "Rules from the first
commit", "Service tree and repo layout", "Scaffold fixes (step 0)", "UI
stack", "Build order" row 0. The scaffold is already on the branch (fresh
`hamr new`, done by the planning session). Nothing in this step touches the
old code or any extract.

Branch: `rewrite`. PR: `rewrite` → `master`. Commit per task, plain messages.

## Tasks

1. **Rename the binaries.** The scaffold ships `cmd/site/`. Make it three
   entry points: `cmd/stackrd` (panel, also `stackrd proxy` later), `cmd/stackr`
   (CLI, empty `main` for now), `cmd/stackr-install` (installer, empty `main`
   for now). Fix the Makefile targets (`build` → `bin/stackrd`, `installcli`,
   `installer`) and the `[[dev.watch]]` rule in `hamr.toml` (`name =
   "stackrd"`, builds and runs `./bin/stackrd`). Any Dockerfile the scaffold
   put under `cmd/site/` moves with `cmd/stackrd`.
   Done when: `make build` produces `bin/stackrd`, `hamr dev` starts it, the
   two other `cmd/` dirs compile.

2. **Fix `AGENTS.md`.** Keep the scaffold's file, fix what contradicts the
   plan: (a) both code examples call a service method, never the store;
   (b) validation: handlers check form shape only (required, type, length),
   services own every domain rule; (c) the store is split by table, one file
   and one small interface per table, no single store interface; (d)
   migrations stay editable until the first real install, the "frozen
   baseline, additive only" rule applies only after that.
   Done when: no example in `AGENTS.md` passes a store to a handler and the
   four points are stated in the file.

3. **Layering rules into `AGENTS.md`.** New section, the ten rules from
   REWRITE.md "Rules from the first commit" in short form, plus the tree from
   "Service tree and repo layout": handlers and `main` see only
   `service.Orchestrator`; store, Docker, proxy, git, s3 wrappers, leaves and
   flows live under `service/internal/`; a leaf reads and writes its own
   table only and never calls another leaf or a flow; a flow computes and
   passes facts down, flow → flow only on the listed edges (promote → deploy,
   deploy → managed); auth is middleware calling `authz.can()`, never a
   handler and never a service; every container op is a job. Include the
   "leaf or flow?" test. Do not mention any old worktree or old code path.
   Done when: the section exists and names every rule above.

4. **Handler-audit skill.** `.claude/skills/handler-audit/SKILL.md`: given a
   handler package (web or API), it greps and reports every store call,
   Docker call, domain decision (a status computed, a permission checked, a
   default chosen) and auth check found inside a handler, with file and line,
   and says what service method should own it. Read-only, no edits. Also a
   `templint` reminder: no `if`/`for` computing a decision in a `.templ`.
   Done when: the skill runs on the scaffold's handlers and prints its report
   (empty findings are fine).

5. **`depguard` in `.golangci.yml`.** Rules: (a) `service/internal/leaf/<a>`
   may not import `service/internal/leaf/<b>`; (b)
   `service/internal/flow/<a>` may not import `service/internal/flow/<b>`
   except the listed edges `flow/promote` → `flow/deploy` and `flow/deploy` →
   `flow/managed`; (c) nothing outside `service/` imports `service/internal/`
   (the compiler does this already; the rule is documentation and catches a
   misplaced package). Keep the scaffold's linters.
   Done when: `make lint` passes and a throwaway sibling-leaf import fails
   lint (then delete the throwaway).

6. **Empty tree.** Create with a `doc.go` (one line) per package so Go
   accepts them: `service/` (`orchestrator.go` with `type Orchestrator
   struct{}` and `func New(cfg Config) (*Orchestrator, error)`),
   `service/internal/{store,docker,proxy,git,s3}`,
   `service/internal/leaf/`, `service/internal/flow/`, `authz/`,
   `ui/components/`, `ui/pages/{org,stack,env,tile}/`, `ui/static/js/`.
   Point `hamr.toml` `[static]` at `ui/static` and the CSS watch rule at
   `ui/css`; move the scaffold's `frontend/` content there and delete
   `frontend/`. The scaffold's web handlers stay where the scaffold put them
   for now; step 6 moves screens into `ui/`.
   Done when: `make build`, `make lint`, `make test` pass with the tree in
   place and `hamr dev` serves the scaffold page.

7. **Done gate.** `make build`, `make lint`, `make test` pass; the skill
   runs; `docs/rewrite/PROGRESS.md` ticked (`[x] step 0 / task N`) and
   committed with the code; PR opened `rewrite` → `master`.

## Not in this step

No schema, no store, no Docker code, no auth code. Those are step 1.
