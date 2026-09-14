---
name: hamr
description: Use this skill when working in a HAMR project or in the HAMR framework repo. Covers the `hamr` CLI, `github.com/FyrmForge/hamr/pkg/*`, and HAMR conventions for handlers, templ, HTMX, and forms. Trigger when the task is specifically about HAMR, the `hamr` command, or code that already uses HAMR packages or conventions.
---

# HAMR

HAMR is a Go full-stack framework for server-rendered HTML apps. The stack is:

- **Go + Echo v4** on the server
- **templ** for compile-time type-safe HTML (NOT Go's `html/template`)
- **HTMX** for server-driven interactivity
- **PostgreSQL** by default, with SQLite support in newer scaffolds
- **hamr** CLI for scaffolding, dev server, codegen, and tooling

Use this skill only when the repo is clearly HAMR-related:

- the project has `hamr.toml`, `docs/llms.txt`, or `docs/llms-full.txt`
- imports include `github.com/FyrmForge/hamr/pkg/...`
- you are working in the upstream `FyrmForge/hamr` repository

## When to load which reference

Keep this file short. Pull in a reference only when the task needs it:

- **`references/cli.md`** — only when you need to run, explain, or troubleshoot a `hamr` subcommand.
- **`references/packages.md`** — only when writing Go that imports from `github.com/FyrmForge/hamr/pkg/*`, or when choosing a HAMR package.
- **`references/practices.md`** — only when editing handler, `.templ`, HTMX, or form code in a HAMR project.

Default to loading no reference files until the task needs one. When in doubt, prefer `references/practices.md` before `references/packages.md`.

## Critical rules (always apply)

1. **`.templ` files are NOT Go's `html/template`.** They compile to Go via the `templ` CLI. Always re-run `templ generate` (or `make build` / `hamr dev`, which do it for you) after editing a `.templ` file — a stale generated `.go` file produces confusing errors.
2. **Handlers render via `respond.HTML(c, status, component)`**, not `c.Render(...)` or raw `c.HTML(...)`. This keeps rendering consistent across HTMX and full-page responses.
3. **HTMX request detection uses `htmx.IsHTMX(c.Request())`**, not manual header checks. Partial-vs-full rendering branches off it.
4. **POST handlers follow PRG**: after a successful write, redirect with `respond.Redirect(c, url)` (which sets `HX-Redirect` for HTMX requests and falls back to 303).
5. **Validators come from `pkg/validate`** — do not roll your own. `validate.NewForm(...)` + `Field(name, rules...)` is the path for form validation; it also renders field errors as HTMX OOB swaps.
6. **Config comes from `pkg/config`** helpers (`GetEnvOrDefault`, `GetEnvOrPanic`, etc.), not ad-hoc `os.Getenv` calls.
7. **Never add JS libraries beyond the vendored set** (`htmx`, `idiomorph`; `alpine` is available via `hamr vendor alpine` when needed). Use `hamr vendor` to manage them; do not check unvendored JS into `frontend/static/js/`.
8. **Build & test go through the Makefile.** Use `make build`, `make test`, `make lint`, `make templint` — never call `go build` / `go test` on individual packages.

## Project layout cheat-sheet

```
cmd/site/                 # main + Dockerfile
internal/
  config/                 # typed env accessors (wraps pkg/config)
  db/                     # DB connection + migrations
  middleware/             # project middleware
  repo/                   # Store interface, per-domain repositories
  web/
    handler/<domain>/     # per-domain handlers and domain-local .templ files
    components/           # shared templ components, static manifest
locales/                  # i18n JSON (if --locale was enabled)
frontend/
  static/                 # raw CSS/JS/images
  css/input.css           # Tailwind source (tailwind projects only)
  dist/                   # fingerprinted static output, committed
  package.json            # npm deps live here, not at the repo root
docs/                     # project docs including llms.txt / llms-full.txt
hamr.toml                 # CLI + dev server + codegen config
```

## Fast lookups

- Full compact framework reference: `docs/llms.txt`
- Full expanded framework reference: `docs/llms-full.txt`
- Human-readable guides: `docs/guide/*.md`
- Per-package deep dives: `docs/guide/pkg/<name>.md`

If the project-local copies aren't present (e.g. working in the hamr repo itself), those same files live under the upstream `FyrmForge/hamr` repository at `llmsdocs/` and `docs/guide/`.

## Sibling skills

If installed alongside this one: `hamr-qa-loop` (iterative QA test-and-fix loop — consider it after finishing a feature) and `hamr-pr-publish` (structured PR with gist-hosted screenshots).
