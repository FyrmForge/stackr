# stackr — Project Conventions

## Build & Test

```bash
make install        # Install dev tools (templ)
# Migrations run automatically when the server starts
hamr dev            # Run dev server (file watching, builds, live reload)
make build          # Build binary (generates templ first)
make test           # Run tests
make lint           # Run linters
make templint       # Lint .templ files for silent failures and a11y issues
```

`T` in the `hamr dev` TUI opens a public tunnel (`[dev.tunnel]` in `hamr.toml`) and sets `BASE_URL` to the public URL for the app it runs. Build absolute URLs from `BASE_URL`; don't hardcode `localhost`.

## Project Structure

```
cmd/stackrd/             Panel entry point (env config loaded here)
cmd/stackr/              CLI entry point
cmd/stackr-install/      Installer entry point
internal/db/            Database connection + embedded migrations
internal/middleware/     Access: session or API key → principal; Require(verb)
internal/web/            HTTP layer
internal/web/server.go   Route registration + middleware groups
internal/web/handler/    One package per page, mirroring URL path
                         (e.g. /admin/users → handler/admin/user/)
internal/web/components/ Shared templ components (layout, form helpers);
                         step 6 moves screens into internal/ui/
internal/service/        The orchestrator; everything below it in internal/
internal/authz/          can(user, verb, resource), used by middleware only
internal/ui/             components/ and pages/{org,stack,env,tile}/ (step 6)
ui/                      Non-Go frontend: static/ (js, css, images), css/
                         source, npm + tailwind config, generated dist/
```

## Layering rules

These are repo rules from the first commit. The compiler enforces most of
them, `depguard` in `make lint` the rest, and the `handler-audit` skill spot
checks handlers.

### Service tree

Handler → orchestrator → flow or leaf → store / infra wrapper. Nothing skips
a level.

```
internal/service/
  orchestrator.go         New(config) and the Orchestrator: the only thing
                          main, the API and the web handlers see. One method
                          per user-facing verb.
  tile.go, org.go, ...    small verbs, one file per area; most are one line
                          calling a leaf
  internal/store/         CRUD, one file and one small interface per table
  internal/docker/        the Docker wrapper, no rules
  internal/proxy/         the Caddy admin client
  internal/git/           clone, checkout
  internal/s3/            backup destinations
  internal/leaf/<name>/   one row kind and the world object it stands for
  internal/flow/<name>/   the big verbs that sequence several leaves
internal/authz/           can(user, verb, resource), called by middleware
internal/ui/              components/ and pages/{org,stack,env,tile}/
ui/static/                static assets (not Go)
```

### The rules

1. **Services are the only home for business logic.** Handlers, the API,
   the CLI, `main`, the store and the Docker package decide nothing about
   the domain.
2. **Handlers and `main` see only `service.Orchestrator`.** The store,
   Docker, proxy, git and s3 wrappers, the leaves and the flows all live
   under `internal/service/internal/`, so Go refuses any import of them
   from outside `internal/service/`.
3. **Dependency injection:** `main` calls `service.New(cfg)`; the service
   builds its own store and Docker client. Tests pass a fake Docker through
   the exported `service.Docker` interface (`service.WithDocker(fake)`).
   Services take a user only for audit fields.
4. **The store is CRUD plus type mapping.** Only schema constraints (FK,
   unique, not null). No defaults, no ordering policy, no status decisions.
5. **The Docker wrapper receives fully resolved specs.** Variables already
   filled in; it never needs a domain rule.
6. **Handlers are dumb.** Bind input, check form shape, call one
   orchestrator method, render. When a screen needs a decision the service
   returns it (e.g. a `CanDeploy` field). `.templ` files never compute a
   decision either.
7. **Errors are typed.** Never branch on an error's wording.
8. **Anything built once is constructed once.** No double wiring in `main`.
9. **Anything not written down behaves as it does today, for domain rules
   only.** The plan (`REWRITE.md`) lists what changes; the extracts in
   `docs/rewrite/extracts/` are the spec for the rest. Mechanics that were
   never ours (rollout, replicas, service DNS, health gating, load
   balancing) are built only as written in the plan, never guessed.
10. **Every container op is a job.** Deploy, promote, rollback, restart,
    stop, backup, image-watch redeploy: all go through `flow/jobs`. Nothing
    starts any of them another way. The orchestrator is the only enqueuer;
    `flow/jobs` gets the flow functions injected by `service.New`, no flow
    imports jobs and jobs imports no flow (DECIDE 12).

### Leaves

- **A leaf is the real thing:** one row kind *and* the world object it
  stands for (`volume` = row + Docker volume, `environment` = row + its
  network, `tile` = row + its containers). Every table has exactly one leaf.
- **A leaf reads and writes its own table only.** Its constructor gets its
  table interface and the slice of the Docker wrapper it needs, nothing
  else. It **never calls another leaf and never a flow**. Facts from other
  tables come in as arguments: `environment.Delete(env, tileCount)`. The
  leaf still owns the decision; the caller only fetches.
- **A leaf runs a spec, never builds one.** `tile.Start(spec)` takes a fully
  resolved spec; spec building lives in `flow/deploy`.

### Flows

- **A flow sequences several leaves.** It computes facts and passes them
  down; it holds no Docker handle (the leaves do).
- **Flow → flow only on the listed edges:** `flow/promote` → `flow/deploy`
  and `flow/deploy` → `flow/managed`. Never a cycle, never a new edge
  without a plan change.
- A flow never enqueues; the orchestrator verb queues the job and the
  worker calls the flow.

### Leaf or flow?

One question: does it stand for one row kind and its world object? **Leaf.**
Does it sequence several? **Flow.**

### Auth

Auth is middleware calling `authz.can(user, verb, resource)`. **Never a
handler, never a service, never a leaf or flow.** Both routers (web and API)
mount the same middleware; handlers below it already have org, stack, env
and tile loaded.

### Enforced how

- `internal/service/internal/...` cannot be imported from outside
  `internal/service/` (compiler).
- Leaf → flow is an import cycle (compiler).
- Leaf → leaf and flow → flow (except the listed edges) fail `make lint`
  (`depguard` in `.golangci.yml`).
- Handlers: run the `handler-audit` skill (`.claude/skills/handler-audit/`).

## Framework Reference

This project uses the HAMR framework (`github.com/FyrmForge/hamr`). Key packages:

- `hamr/pkg/server` — Echo wrapper with functional options
- `hamr/pkg/respond` — HTTP response helpers (HTML/JSON/Redirect)
- `hamr/pkg/validate` — Validators + Form API (define rules once, validate full form & per-field)
- `hamr/pkg/middleware` — Auth, CSRF, flash, rate limiting, etc.
- `hamr/pkg/db/sqlite` — SQLite connection with pragmas + migrations
- `hamr/pkg/config` — Environment variable helpers
- `hamr/pkg/logging` — Structured logging (slog)
- `hamr/pkg/htmx` — HTMX request/response helpers

See `docs/llms.txt` for a compact API reference and `docs/llms-full.txt` for
complete package documentation.

## Handler Pattern

One Go package per page, with the directory tree mirroring the URL path.
A page package owns its main handler, its template files, and any HTMX
helper routes that belong to it (validation endpoints, partials, modals).

```
URL                     Package path
/                       internal/web/handler/home
/login                  internal/web/handler/auth/login
/register               internal/web/handler/auth/register
/admin                  internal/web/handler/admin
/admin/users            internal/web/handler/admin/user
/admin/users/:id/edit   internal/web/handler/admin/user/edit
```

URL segments are typically plural (`/users`); package names are singular Go
identifiers (`user`). When a parent path doesn't have its own page, the
parent directory still exists but contains no `handler.go`.

Pure-action endpoints with no view (e.g. `POST /logout`) hang off the most
related page package — Logout lives in `handler/auth/login/` because it's
the inverse of Login. Helpers shared across page packages in a section
(e.g. session-cookie helpers used by login + register) live outside the
handler tree, in `internal/auth/` or similar.

### Creating a New Page Package

1. Create a directory at `internal/web/handler/<path>/<page>/`
2. `handler.go` (package `<page>`) — `NewHandler(orch)` returning `*handler`,
   methods like `Page` (GET) and `Submit` (POST), plus any HTMX helper methods.
   The handler's only dependency is `*service.Orchestrator`; it never holds a
   store, a Docker client or any other service.
3. `<page>.templ` (same package) — the page's templates
4. Register routes in `internal/web/server.go` — page route + every HTMX
   helper route the page exposes

### Example

```go
package things

import (
    "net/http"

    "github.com/FyrmForge/hamr/pkg/respond"
    "github.com/labstack/echo/v4"

    "github.com/FyrmForge/stackr/internal/service"
)

type handler struct {
    orch      *service.Orchestrator
    FormRules validate.Form
}

func NewHandler(orch *service.Orchestrator) *handler {
    return &handler{orch: orch, FormRules: newFormRules()}
}

// GET /things
func (h *handler) Page(c echo.Context) error {
    things, err := h.orch.ListThings(c.Request().Context())
    if err != nil {
        return err
    }
    return respond.HTML(c, http.StatusOK, ThingsPage(c, toView(things)))
}

// POST /things
func (h *handler) Submit(c echo.Context) error {
    var f CreateForm
    if err := c.Bind(&f); err != nil {
        return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
    }

    // Form shape only (required, type, length). Domain rules live in the service.
    if errs := h.FormRules.Validate(c); errs != nil {
        return respond.HTML(c, http.StatusUnprocessableEntity, createForm(c, f, errs))
    }

    // One service call. The service decides, checks domain rules and saves.
    if err := h.orch.CreateThing(c.Request().Context(), f.Name); err != nil {
        return err
    }

    middleware.SetFlash(c, "Created successfully!", middleware.FlashSuccess)
    return respond.Redirect(c, "/things")
}
```

### Response Functions

- `respond.HTML(c, status, component)` — Render a templ component
- `respond.JSON(c, status, data)` — Return JSON
- `respond.Redirect(c, url)` — HTMX-aware (sets HX-Redirect for HTMX, 303 otherwise)
- `echo.NewHTTPError(code, msg)` — Return an error (caught by ErrorPages middleware)

### Status Codes

- `200` — Successful GET
- `303` — Redirect after POST (PRG pattern)
- `400` — Bad request
- `401` — Unauthorized
- `403` — Forbidden
- `404` — Not found
- `422` — Validation error
- `500` — Server error

## Forms & Validation

### Validation Basics

Use `hamr/pkg/validate` — all validators return `""` for valid, error string
for invalid. Every validator has a `*Msg` variant for custom messages.

```go
validate.Required(value)          // "This field is required"
validate.Email(value)             // "Invalid email address"
validate.MinLength(value, 8)      // "Must be at least 8 characters"
validate.MaxLength(value, 100)    // "Must be at most 100 characters"
validate.PasswordStrength(value)  // Checks upper, lower, digit, special, length
validate.URL(value)               // "Invalid URL"
validate.Phone(value)             // "Invalid phone number"
validate.OneOf(value, "a", "b")   // "Must be one of: a, b"
```

Character-class validators (composable password rules):

```go
validate.HasUpper(value)    // contains uppercase letter
validate.HasLower(value)    // contains lowercase letter
validate.HasDigit(value)    // contains digit
validate.HasSpecial(value)  // contains special character
```

Curried constructors return `func(string) string` for use as Form rules:

```go
validate.MinLen(3)               // func(string) string
validate.MaxLen(100)             // func(string) string
validate.In("admin", "user")    // func(string) string
validate.AgeMin(18)              // func(string) string
```

### Who validates what

- **Handlers check form shape only:** required, type (email, number, URL),
  length. Nothing that needs the database or a domain fact.
- **Services own every domain rule:** uniqueness, slug grammar, "may this
  env be deleted", permissions-derived defaults, status. The service returns
  a typed error; the handler maps it to a field error or a status code.
- **The store validates nothing** beyond schema constraints (FK, unique, not
  null).

### Two-Level Validation

1. **Blur** (inline): HTMX `hx-post` to validate a single field, return OOB swap
2. **Submit** (full): Validate all fields, return 422 with form re-render

### Form API — Define Rules Once

Define form structs with `form:` tags and validation rules in the handler
constructor. Use `c.Bind()` to populate the struct, then `Validate(c)` to check:

```go
type CreateForm struct {
    Name  string `form:"name"`
    Email string `form:"email"`
}

type Handler struct {
    orch            *service.Orchestrator
    CreateFormRules validate.Form
}

func NewHandler(orch *service.Orchestrator) *Handler {
    return &Handler{
        orch: orch,
        CreateFormRules: validate.NewForm(
            validate.WithOOBRenderer(form.OOBValidator),
            validate.WithGeneralError("Please fix the errors below."),
            validate.WithTrim(true),
            validate.Field("name", validate.Required, validate.MinLen(2)),
            validate.Field("email", validate.Required, validate.Email),
        ),
    }
}

// POST /things
func (h *Handler) Create(c echo.Context) error {
    var f CreateForm
    if err := c.Bind(&f); err != nil {
        return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
    }

    // Shape check: required, email format.
    if errs := h.CreateFormRules.Validate(c); errs != nil {
        return respond.HTML(c, http.StatusUnprocessableEntity, createForm(c, f, errs))
    }

    // Domain rules (e.g. "email already taken") come back as typed errors.
    err := h.orch.CreateThing(c.Request().Context(), f.Name, f.Email)
    if errors.Is(err, service.ErrEmailTaken) {
        errs := map[string]string{"email": "Email already registered"}
        return respond.HTML(c, http.StatusUnprocessableEntity, createForm(c, f, errs))
    }
    if err != nil {
        return err
    }

    middleware.SetFlash(c, "Created successfully!", middleware.FlashSuccess)
    return respond.Redirect(c, "/things")
}
```

Field definitions support three levels of error messages:

```go
// Level 1 — Default messages from each rule
validate.Field("email", validate.Required, validate.Email)

// Level 2 — One message for any failure on the field
validate.FieldMsg("email", "Email is invalid", validate.Required, validate.Email)

// Level 3 — Per-rule message override
validate.Field("email",
    validate.Required,
    validate.WithMsg(validate.Email, "Please enter a valid email"),
)
```

Context-aware rules for cross-field validation (still form shape: two fields
of the same form agreeing, never a lookup):

```go
validate.Field("password_confirm", validate.Required).
    WithCtx(func(c echo.Context, value string) string {
        if value != c.FormValue("password") {
            return "Passwords do not match"
        }
        return ""
    })
```

### HTMX Per-Field Validation

Register one route — `ValidationHandler` handles all fields automatically:

```go
// In server.go route registration:
group.POST("/things/validate/:field", h.CreateFormRules.ValidationHandler("field"))
```

No need for manual switch statements. Unknown fields return an empty response.

The `form.OOBValidator` helper renders OOB error swaps:

```go
func OOBValidator(c echo.Context, field, errMsg string) error {
    status := http.StatusOK
    if errMsg != "" {
        status = http.StatusUnprocessableEntity
    }
    return respond.HTML(c, status, FieldErrorOOB(field, errMsg))
}
```

A field can override the form-level renderer (e.g. password requirements checklist):

```go
validate.Field("password", validate.Required, validate.PasswordStrength).
    WithRenderer(form.PasswordRequirementsRenderer)
```

### Form Template Pattern

Separate the **page** (wraps layout) from the **form** (what HTMX swaps).
Pass the form struct and errors map:

```
templ createForm(c echo.Context, f CreateForm, errors map[string]string) {
    <form
        id="create-form"
        hx-post="/things"
        hx-swap="outerHTML"
        hx-target="#create-form"
        method="POST"
        action="/things"
    >
        @form.CSRFField(c)
        <div class="form-group">
            <label for="name">Name</label>
            <input type="text" id="name" name="name" value={ f.Name }
                hx-post="/things/validate/name"
                hx-trigger="blur, hamr:revalidate"
                hx-swap="none"/>
            @form.FieldError("name", form.GetError(errors, "name"))
        </div>
        <button type="submit" class="btn btn-primary">Create</button>
    </form>
}
```

Key HTMX attributes:
- `hx-post` — Submit via HTMX
- `hx-swap="outerHTML"` — Replace the entire form on validation errors
- `hx-target="#create-form"` — Target the form element
- `hx-swap="none"` on inputs — Field validation uses OOB swaps, no explicit target
- `hx-trigger="blur, hamr:revalidate"` — Validate on blur; re-validate while typing only if an error is showing
- `hamr:revalidate` — Custom event fired by `static/js/main.js`, 300ms after the last keystroke, and only while the field's `error-<name>` span has `data-has-error="true"`. Do NOT use an `hx-trigger` `[...]` filter for this: htmx compiles those with `Function()`, which the CSP blocks. Add `data-hamr-watch="other_field"` to gate on another field's error instead.

### Field Error Components

- `form.FieldError(field, err)` — Renders inline; always present in DOM for OOB targeting
- `form.FieldErrorOOB(field, err)` — Same but with `hx-swap-oob="true"` for blur validation
- `form.GetError(errors, field)` — Safe map lookup, returns `""` if nil or missing
- `form.OOBValidator(c, field, errMsg)` — Default OOB renderer for `ValidationHandler`

### CSRF Token

The CSRF middleware stores the token in `c.Get("csrf")`. Pass `echo.Context`
to templ components and use `@form.CSRFField(c)` — the component extracts the
token internally. The `htmx:configRequest` listener in `layout.templ`
automatically attaches it to HTMX requests via the `X-CSRF-Token` header.

### Flash Messages

```go
middleware.SetFlash(c, "Saved!", middleware.FlashSuccess)
return respond.Redirect(c, "/things")
```

Read in templates via `middleware.GetFlash(c)` — returns `*middleware.FlashMessage`
with `.Message` and `.Type` fields. Layout renders these automatically.

## Responses

Use `respond.Redirect` for HTMX-aware redirects — it sets `HX-Redirect` for
HTMX requests and falls back to 303 for regular requests.

On validation errors, return the **form component** (not the full page) with
status 422. HTMX swaps the form in-place; for non-JS fallback the full page
renders.

## CSS

Tailwind CSS — classes directly in templ components. Config in `ui/tailwind.config.js`.

```bash
make install     # installs npm deps in ui/
make css-build   # one-shot production build
```

`hamr dev` rebuilds the CSS on every `.templ` change — there is no watch
daemon.

```
ui/css/input.css          Tailwind directives (@tailwind base, components, utilities)
ui/static/css/output.css  Generated CSS (do not edit)
ui/dist/                  Fingerprinted output of `hamr gen static`
```

Custom components via `@apply` in `ui/css/input.css`:

```css
@layer components {
    .btn-primary {
        @apply px-4 py-2 bg-blue-600 text-white rounded hover:bg-blue-700;
    }
}
```

## Templ Linting

- Run `make templint` to lint `.templ` files
- Control flow on one line (`if cond { <div>...</div> }`) is silently dropped by templ — always use multiline
- Accessibility: `<img>` must have `alt=`, `<a>` must have `href=`
- Style: avoid inline `style=` attributes, empty `class=""`, and `href="javascript:..."`
- Configure rules via `[lint.templ]` in `hamr.toml`

## Testing

- Unit tests alongside source files: `*_test.go`
- stdlib `testing` only, table-driven; no testify
- Real SQLite in `t.TempDir()`, never mocks: `servicetest.New(t)` gives an
  orchestrator with the Docker fake and seed helpers; `servicetest.Store(t)`
  gives a bare migrated store for store, leaf and flow tests

## Database
- Migrations in `internal/db/migrations/` (sequential numbering)
- Use `sqlx` for queries in the store
- Migrations run during server startup via `db.Migrate(...)`
- **The store is split by table.** It lives in
  `internal/service/internal/store/`: one file and one small interface per
  table (`tiles.go` → `TileStore`), one shared `Tx` so a flow can write
  several tables in one transaction. There is no single store interface. A
  table file only writes SQL against its own table; a join lives in the
  query file of the flow that owns the question, with a one-line reason.
- **Migrations stay editable until the first real install.** Until then,
  edit `001_initial` in place instead of stacking fix-up migrations. After
  the first real install the baseline is frozen and changes are additive
  only (new numbered migrations).

## Auth

Session-based authentication using `hamr/pkg/auth` and `hamr/pkg/middleware`.

### Middleware Wiring

Middleware is configured in `internal/web/server.go`:

- `access.Load()` — group-level on both routers: a bearer API key or the
  session cookie becomes a `*service.Principal` (the only DB call)
- `access.Require(verb)` — per-route: resolves `:org/:stack/:env/:tile`,
  checks `authz.Can`; 401 anonymous, 404 not a member, 403 too low
- `auth.RequireAuth()` — per-route, redirects unauthenticated users to login
- `auth.RequireNotAuth()` — per-route, redirects authenticated users away from login/register

### Handler Pattern

Login and register each get their own page package
(`internal/web/handler/auth/login/`, `internal/web/handler/auth/register/`).
Logout is a sibling action on the login package — it's the inverse of login,
not its own page. Session-cookie helpers shared between login and register
live in `internal/auth/`.

```go
// Login handler — POST /login
func (h *handler) Submit(c echo.Context) error {
    var f LoginForm
    if err := c.Bind(&f); err != nil {
        return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
    }

    session, err := h.svc.Login(c.Request().Context(), f.Email, f.Password)
    if _, bad := errs.IsInvalid(err); bad { /* return form error */ }
    if err != nil { /* return 500 */ }

    auth.SetSession(c, h.svc.Sessions(), session)  // from github.com/FyrmForge/stackr/internal/auth
    return respond.Redirect(c, "/")
}
```

### Key Functions

- `auth.HashPassword(password) (string, error)` — Argon2id hashing
- `auth.CheckPassword(password, hash) (bool, error)` — verify password
- `middleware.GetSubjectID(c) string` — get authenticated user ID
- `middleware.GetSubject(c) any` — get loaded user object (needs SubjectLoader)

## Environment

- `DATA_DIR` — database, job logs and other state (default `./data`)
- `DATABASE_PATH` — overrides `$DATA_DIR/stackr.db`
- `STACKR_MASTER_KEY` — 64 hex chars, encrypts secrets at rest. Required;
  stackrd refuses to start without it and never generates one (the
  installer writes it). Dev: `openssl rand -hex 32` into `.env`.

## Code Style

- Follow existing patterns in the codebase
- Use `hamr/pkg` helpers instead of reimplementing
- Prefer `respond.HTML`/`respond.JSON` over raw `c.HTML()`
- Add `// GET /path` comments above handler methods
- Keep handlers thin — business logic in service layer (see "Layering rules")
