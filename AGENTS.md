# stackr — Project Conventions

## Replying

- Answer in short bullet points. No walls of prose.
- One idea per bullet, shortest form that is still clear.
- Prose paragraphs only when explicitly asked for (a written plan, a doc, a
  commit message).
- Lead with the outcome bullet, detail below it.
- Don't restate the question, don't recap what was already agreed.

## Project Stage

- Pre-release. There are no production installs or compatibility promises.
- Test deployments are disposable. Their addresses and credentials belong in
  local environment variables, never in tracked files.
- Remote test helpers require `STACKR_DEPLOY_HOST` and live under
  `scripts/dev/`:

  ```bash
  STACKR_DEPLOY_HOST=manager.example.test \
      ./scripts/dev/wipe-test.sh --nodes worker.example.test
  STACKR_DEPLOY_HOST=manager.example.test ./scripts/dev/deploy-test.sh
  ```

- No backwards compatibility, no data migrations for existing installs, no
  upgrade paths. Break APIs and config freely; a test VM is wiped and
  reinstalled.
- Do not raise "what about existing users" questions.
- The one exception is the database schema: `001_initial` is frozen as of
  2026-09-18. See Database below.

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

### Shell calls that stall an unattended run

These shapes trip the permission classifier and stop the run dead waiting for
a human. On a long autonomous task that is the difference between finishing
and burning the night on one prompt.

Do not use:

- `sed -i` — any in-place edit
- heredocs of any kind, including `python - <<'PY'` and `git commit -F -`
- `git rm -f`
- long `&&` chains, especially ones mixing reads with writes

Use instead:

- the Edit and Write tools for every file change, test fixtures and generated
  allowlists included
- `git commit -F <file>`, with the message written to a scratchpad file first
- one plain command per call

`git push` and `gh pr create` prompt regardless — they reach outside the
machine. Everything else on that list is avoidable.

## Project Structure

```
cmd/stackrd/              Application entry point (env config loaded here)
internal/stackrd/store/db/            Database connection + embedded migrations
internal/stackrd/store/repo/           Data access layer (Store interface + SQLite impl)
internal/stackrd/handlers/web/            HTTP layer
internal/stackrd/handlers/web/server.go   Route registration + middleware groups
internal/stackrd/handlers/web/handler/    One package per page, mirroring URL path
                         (e.g. /admin/users → handler/admin/user/)
internal/stackrd/handlers/web/components/ Shared templ components (layout, form helpers)
frontend/                Everything frontend: static assets, CSS source,
                         npm config, generated dist/
```

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

See the [HAMR repository](https://github.com/FyrmForge/hamr) for framework
documentation matching the version in `go.mod`.

## Handler Pattern

One Go package per page, with the directory tree mirroring the URL path.
A page package owns its main handler, its template files, and any HTMX
helper routes that belong to it (validation endpoints, partials, modals).

```
URL                     Package path
/:org                   internal/stackrd/handlers/web/handler/org
/:org/:stack            internal/stackrd/handlers/web/handler/project
/servers/:id            internal/stackrd/handlers/web/handler/server
/admin/proxy            internal/stackrd/handlers/web/handler/settings
/tiles/:id/backups      internal/stackrd/handlers/web/handler/backups
```

(The live list is the directory itself —
`internal/stackrd/handlers/web/handler/`.)

URL segments are typically plural (`/users`); package names are singular Go
identifiers (`user`). When a parent path doesn't have its own page, the
parent directory still exists but contains no `handler.go`.

Pure-action endpoints with no view (e.g. `POST /logout`) hang off the most
related page package — Logout lives in `handler/auth/login/` because it's
the inverse of Login. Helpers shared across page packages in a section
(e.g. session-cookie helpers used by login + register) live outside the
handler tree, in `internal/stackrd/handlers/web/handler/auth/` or similar.

### Creating a New Page Package

1. Create a directory at `internal/stackrd/handlers/web/handler/<path>/<page>/`
2. `handler.go` (package `<page>`) — `NewHandler(deps)` returning `*handler`,
   methods like `Page` (GET) and `Submit` (POST), plus any HTMX helper methods
3. `<page>.templ` (same package) — the page's templates
4. Register routes in `internal/stackrd/handlers/web/server.go` — page route + every HTMX
   helper route the page exposes

### Example

```go
package things

import (
    "net/http"

    "github.com/FyrmForge/hamr/pkg/respond"
    "github.com/labstack/echo/v4"

    "github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
    store repo.Store
}

func NewHandler(store repo.Store) *handler {
    return &handler{store: store}
}

// GET /things
func (h *handler) Page(c echo.Context) error {
    return respond.HTML(c, http.StatusOK, ThingsPage(c))
}

// POST /things
func (h *handler) Submit(c echo.Context) error {
    var f CreateForm
    c.Bind(&f)

    if errs := h.FormRules.Validate(c); errs != nil {
        return respond.HTML(c, http.StatusUnprocessableEntity, createForm(c, f, errs))
    }

    // Save to database...

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

### Two-Level Validation

1. **Blur** (inline): HTMX `hx-post` to validate a single field, return OOB swap
2. **Submit** (full): Validate all fields, return 422 with form re-render

Never validate in the repo/store layer.

### Form API — Define Rules Once

Define form structs with `form:` tags and validation rules in the handler
constructor. Use `c.Bind()` to populate the struct, then `Validate(c)` to check:

```go
type CreateForm struct {
    Name  string `form:"name"`
    Email string `form:"email"`
}

type Handler struct {
    store          repo.Store
    CreateFormRules validate.Form
}

func NewHandler(store repo.Store) *Handler {
    return &Handler{
        store: store,
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
    c.Bind(&f)

    if errs := h.CreateFormRules.Validate(c); errs != nil {
        return respond.HTML(c, http.StatusUnprocessableEntity, createForm(c, f, errs))
    }

    // Save to database...
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

Context-aware rules for cross-field validation:

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
                hx-trigger="blur, input[this.closest('.form-group').querySelector('[data-has-error=true]')] delay:300ms"
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
- `hx-trigger` — Validate on blur; re-validate on input only if an error is showing

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

Tailwind CSS — classes directly in templ components. Config in `frontend/tailwind.config.js`.

```bash
make install     # installs npm deps in frontend/
make css-build   # one-shot production build
```

`hamr dev` rebuilds the CSS on every `.templ` change — there is no watch daemon.

```
frontend/css/input.css          Tailwind directives (@tailwind base, components, utilities)
frontend/static/css/output.css  Generated CSS (do not edit)
frontend/dist/                  Fingerprinted output of `hamr gen static` (gitignored)
```

Custom components via `@apply` in `frontend/css/input.css`:

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
- Use `testify/assert` and `testify/require`

## Database
- Migrations in `internal/stackrd/store/db/migrations/` (sequential numbering)
- **`001_initial` is frozen (2026-09-18).** A schema change is a new `002_*`
  on top of it, never an edit to the baseline. Up to that date every change
  edited the baseline and the rig got wiped; that is over, because the next
  install may hold data nobody can recreate.
- Migrations after the baseline are **additive only**: no `DROP`, no `RENAME`,
  no `TRUNCATE`, no `DELETE FROM`. `migrate_guard_test.go` enforces it, and
  pins the baseline's hash so an edit to `001_initial` fails the build. A deliberate drop needs
  a `-- migration-guard: allow <reason>` line above the statement, and is
  normally two releases: stop writing the column, remove it once no running
  version reads it.
- Use `sqlx` for queries in repo implementations
- Migrations run during server startup via `db.Migrate(...)`
- Store interface in `internal/stackrd/store/repo/repo.go`

## Auth

Session-based authentication using `hamr/pkg/auth` and `hamr/pkg/middleware`.

### Middleware Wiring

Middleware is configured in `internal/stackrd/handlers/web/server.go`:

- `auth.Load()` — group-level, populates context from session (the only DB call)
- `auth.RequireAuth()` — per-route, redirects unauthenticated users to login
- `auth.RequireNotAuth()` — per-route, redirects authenticated users away from login/register

### Handler Pattern

Login and register each get their own page package
(`internal/stackrd/handlers/web/handler/auth/login/`, `internal/stackrd/handlers/web/handler/auth/register/`).
Logout is a sibling action on the login package — it's the inverse of login,
not its own page. Session-cookie helpers shared between login and register
live in `internal/stackrd/handlers/web/handler/auth/`.

```go
// Login handler — POST /login
func (h *handler) Submit(c echo.Context) error {
    var f LoginForm
    if err := c.Bind(&f); err != nil {
        return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
    }

    user, err := h.authService.Authenticate(c.Request().Context(), f.Email, f.Password)
    if err != nil { /* return form error */ }

    session, err := h.sessionManager.CreateSession(c.Request().Context(), user.ID, nil)
    if err != nil { /* return 500 */ }

    auth.SetSession(c, h.sessionManager, session)  // from github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/auth
    return respond.Redirect(c, "/")
}
```

### Key Functions

- `auth.HashPassword(password) (string, error)` — Argon2id hashing
- `auth.CheckPassword(password, hash) (bool, error)` — verify password
- `middleware.GetSubjectID(c) string` — get authenticated user ID
- `middleware.GetSubject(c) any` — get loaded user object (needs SubjectLoader)

## Storage

Pluggable file storage with `hamr/pkg/storage`:

- `storage.FileStorage` interface: `Save`, `Open`, `Delete`, `Exists`, `List`
- `storage.NewLocalStorage(basePath)` — local filesystem backend
- `storage.NewS3Storage(cfg)` — S3-compatible backend (AWS, RustFS, R2)

### Environment Variables
- `STORAGE_PATH` — local directory for file uploads

## WebSockets

WebSocket support via `hamr/pkg/websocket`:

### Hub Setup

```go
hub := websocket.NewHub()
defer hub.Close()
```

If subject-based routing is needed, pass `websocket.WithSubjectIDFunc(...)` and
derive the subject ID from request data available during the WebSocket upgrade.

### Sending Messages

```go
emitter := websocket.NewEmitter(hub)

// Send HTML to a specific user
emitter.ToSubject(userID, websocket.NewHTMLEvent("update", "#target", htmlStr))

// Broadcast to a room
emitter.ToRoom("chat", websocket.NewEvent("message", payload))

// Trigger HTMX event
emitter.ToSession(sessionID, websocket.NewTriggerEvent("refresh", "#list", "reload"))
```

### Rooms

```go
hub.JoinRoom(client, "chat:123")
hub.LeaveRoom(client, "chat:123")
hub.SendToRoom("chat:123", msg)
```

### Event Types

1. **HTML Direct**: set Target + HTML — client swaps HTML into target
2. **HTMX Trigger**: set Target + Trigger — client calls htmx.trigger()
3. **Data Only**: set Payload — client handles via registered callback

## Code Style

- Follow existing patterns in the codebase
- Use `hamr/pkg` helpers instead of reimplementing
- Prefer `respond.HTML`/`respond.JSON` over raw `c.HTML()`
- Add `// GET /path` comments above handler methods
- Keep handlers thin — business logic in service layer

## hamr MCP

`hamr dev` exposes these tools over MCP. Prefer them over doing the same
thing by hand — they read the live dev server, so their answers are current
and cost the developer nothing.

- Never ask the developer to paste logs, and never tail a log file — `logs.read` (app + build output), `console.read` (browser console, uncaught errors, CSP violations), `http.read` (request log).
- The dev server is already running. Never run `make build`, `go build`, or start a second server — `rule.run` rebuilds one watch rule, `rebuild.all` rebuilds everything, `make.run` runs a Makefile target.
- Check dependency containers with `docker.status` / `docker.logs` before assuming a connection error is app-side.
- `docker.restart` restarts a service; `docker.wipe` resets its volumes.
- Never ask what an email said — `mail.list` and `mail.get` read the dev inbox.
- `mail.clear` empties it; `mail.ingest` injects a message.
- Never ask what an SMS said — `sms.list` and `sms.get` read the dev inbox.
- `sms.clear` empties it; `sms.ingest` injects a message.
- Never guess at payment state — `stripe.list` reads the mock's objects.
- `stripe.complete` / `stripe.expire` / `stripe.refund` drive a payment to an outcome.
- `dev.info` reports the running rules, ports (including walked ones), and versions — read it before assuming a port.

If a call fails with "dev not running / gateway off", say so instead of
falling back to manual steps — the developer needs to start `hamr dev`.
