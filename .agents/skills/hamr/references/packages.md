# hamr Packages

All importable from `github.com/FyrmForge/hamr/pkg/<name>`. Groups mirror the source layout: **core** packages are used in almost every project; **utility** packages are opt-in; **optional** packages activate based on scaffold flags (websockets, e2e, auth, etc.).

Full signatures: see `docs/llms.txt` (compact) or `docs/llms-full.txt` (expanded) in the project, or `llmsdocs/` upstream.

---

## Core

### `config`
Typed env-var accessors. Use these instead of bare `os.Getenv`.

- `GetEnvOrDefault(key, def string) string`
- `GetEnvOrPanic(key string) string` — for required values; fails fast at startup
- `GetEnvOrDefaultInt`, `GetEnvOrDefaultBool`, `GetEnvOrDefaultDuration`
- `ParseBaseURL(raw string) (origin, hostname string, err error)`

### `server`
Echo v4 wrapper with functional options and graceful shutdown.

- `New(opts ...Option) (*Server, error)`
- Options: `WithHost`, `WithPort`, `WithDevMode`, `WithMiddleware`, `WithStaticDir`, `WithStaticDistDir`, `WithEmbeddedStatic`, `WithErrorHandler`, `WithTimeout`, `WithMaxBodySize`, `WithShutdownTimeout`, `WithGeneratedDir`
- Methods: `Echo()`, `Addr()`, `Start()`, `Shutdown(ctx)`, `GET/POST/PUT/DELETE/PATCH`, `Group(prefix, mw...)`, `StaticPage(path, handler)`, `GenerateStatic(dir)`

### `logging`
Context-aware slog wrapper. Always resolve the logger via context so request IDs and subject IDs propagate.

- `New(production bool) *slog.Logger`
- `FromContext(ctx) *slog.Logger`
- `WithLogger(ctx, l) context.Context`
- `With(ctx, args...) context.Context` — attach key/values to the ctx logger

### `ctx`
Type-safe Echo context keys using generics. Prefer these over raw string keys.

- `NewKey[T](name) Key[T]`
- `Set[T](c, key, value)`, `Get[T](c, key) (T, bool)`, `MustGet[T](c, key) T`
- Predefined: `SubjectIDKey`, `SubjectKey`, `SessionKey`, `RequestIDKey`, `FlashKey`, `TranslatorKey`, `LocaleKey`

### `respond`
HTTP response helpers. **All handlers render via this package.**

- `HTML(c, status, component) error` — renders a `templ.Component`
- `JSON(c, status, data) error`
- `Redirect(c, url) error` — HTMX-aware: sets `HX-Redirect` for HTMX, 303 otherwise
- `ParsePagination(c, defaultSize) (page, size int)`
- `NewPage(page, size, total) Page`

### `validate`
Composable validators. Use this for forms, API inputs, and anywhere you'd otherwise write ad-hoc checks.

- Core: `Required`, `Email`, `Phone`, `URL`, `PasswordStrength`
- Multi-arg: `MinLength`, `MaxLength`, `OneOf`, `IntRange`, `MinAge`, `MaxAge`
- Password rule helpers: `HasUpper`, `HasLower`, `HasDigit`, `HasSpecial`, `CheckPasswordRequirements`
- Curried constructors returning `Rule`: `MinLen(n)`, `MaxLen(n)`, `In(vals...)`, `AgeMin(n)`, `AgeMax(n)`
- Types: `type Rule = func(string) string`, `type CtxRule = func(echo.Context, string) string`
- `WithMsg(rule, msg)` — wrap a rule with a custom message
- `RunRules(value, rules...)` — runs in order, returns first error
- Custom validator registry: `Register(name, fn)`, `Run(name, value)`
- **Form API** (preferred for HTML forms):
  - `NewForm(opts...) Form`, `Field(name, rules...)`, `FieldMsg(name, msg, rules...)`
  - `Form.Validate(c) map[string]string`
  - `Form.ValidationHandler(paramName) echo.HandlerFunc` — HTMX inline field validation endpoint
  - `FieldBuilder.WithRenderer(fn)`, `WithCtx(rules...)` — `CtxRule`s run after standard rules and only if all pass; reads via `c.FormValue("other_field")` are untrimmed regardless of `WithTrim`
  - Cross-field via HTMX: `ValidationHandler` only sees the triggering input's value by default — pull other fields in with `hx-include="[name='other_field']"` on the input or its trigger won't have access to `c.FormValue("other_field")`
  - Options: `WithOOBRenderer(fn)`, `WithGeneralError(msg)`, `WithTrim(bool)`, `WithShortCircuit(bool)`

### `htmx`
HTMX request detection and response header helpers.

- Request: `IsHTMX(r)`, `IsBoosted(r)`, `GetTrigger(r)`, `GetTarget(r)`
- Response: `Redirect(w, url)`, `Trigger(w, events...)`, `TriggerAfterSettle`, `TriggerAfterSwap`, `Reswap(w, strategy)`, `Retarget(w, selector)`, `Refresh(w)`, `PushURL(w, url)`, `ReplaceURL(w, url)`

### `db`
PostgreSQL (and SQLite) connection with retry + golang-migrate integration.

- `Connect(databaseURL, opts...) (*sqlx.DB, error)` / `ConnectContext(ctx, ...)`
- Options: `WithMaxOpenConns`, `WithMaxIdleConns`, `WithConnMaxIdleTime`, `WithConnMaxLifetime`, `WithMaxRetries`, `WithAttemptTimeout`, `WithPgBouncerSafe`, `WithLogger`, `WithOnRetry`
- Migrations: `Migrate(db, cfg)`, `MigrateDown`, `MigrateSteps`, `MigrateVersion`, `MigrateForce`
- Keep-alive: `StartKeepAlive(ctx, db, interval, _)`, `StartKeepAliveWithConfig(ctx, db, cfg)`

### `middleware`
HTTP middleware. Most projects wire a subset of these in `cmd/site/main.go`.

- **Error pages**: `ErrorPages(defaultPage, overrides...)`, `Page(code, page)` — per-group error rendering
- **Auth**: `NewBrowserAuth(sm, opts...)`, `Load()`, `RequireAuth()`, `RequireNotAuth()`
- **Subject**: `GetSubjectID(c)`, `GetSubject(c)`, `TrustedSubject()`
- **CSRF**: `CSRF()`, `CSRFWithConfig(cfg)`
- **CORS**: `CORS()`, `CORSWithConfig(cfg)`
- **Flash**: `Flash()`, `FlashWithConfig(cfg)`, `SetFlash(c, msg, flashType)`, `GetFlash(c)`
- **Security**: `Secure()`, `SecureWithConfig(cfg)`, `CacheControl(disableCaching)`, `CacheControlWithConfig(cfg)` — fingerprint-aware (hashed URLs get `immutable`)
- **RBAC**: `RequireRoles(checker, roles...)`, `RequireActive(checker)`
- **Audit**: `Audit(logger)`, `AuditWithConfig(cfg)`
- **Rate limit**: `RateLimit(store)`, `RateLimitWithConfig(cfg)`, `NewMemoryStore(opts...)`, `NewPGStore(db)`
- **Locale**: `LocaleFromPath(cfg)`, `LocaleFromPreference(cfg)`, `GetLocale(c)`, `GetDirection(c)`

### `ptr`
Generic pointer helpers — useful for nullable DB columns and optional JSON fields.

- `To[T](v) *T`, `From[T](p) T`, `FromOr[T](p, def) T`
- Formatters: `String(p)`, `Int(p)`, `Bool(p)`, `IntToStr(p)`, `BoolToYesNo(p)`

---

## Utility

### `janitor`
Cron-based background task runner.

- `Task` interface: `Name() string`, `Run(ctx) (int64, error)`
- `New(opts...) *Janitor`, `AddTask(schedule, task) *Janitor`
- `Start(ctx) error`, `Stop()`
- Options: `WithTimeout`, `WithLogger`, `WithPreRun`, `WithPostRun`, `WithPreTick`, `WithPostTick`, `WithRunImmediately`

### `async`
Concurrency primitives with built-in panic recovery.

- `All(ctx, fns...) error` — first error cancels remaining
- `Settle(ctx, fns...) []error` — wait for all
- `Map[T,R](ctx, items, fn) ([]R, error)` — concurrent map
- `Fire(fn)` — fire-and-forget with panic guard
- `NewGroup(opts...) *Group`, `Go(fn)`, `Close()` — managed goroutine pool
- Options: `WithGroupLogger`, `WithLimit(n)`, `WithMetrics(m GroupMetrics)`
- `GroupMetrics` interface: `Blocked`, `Dispatched`, `Completed`, `Panicked` — **must not block, do I/O, or panic** (synchronous on the hot path)

### `templint`
Library backing `hamr lint templ`. Also usable programmatically.

- `New(cfg *Config) *Linter` — nil config = no rules run (must list rules in `[lint.templ]`)
- `LintFile(path)`, `LintDir(dir)`
- `FilterBySeverity(diags, minSev)`, `HasErrors(diags)`
- `LoadConfig(path)` — reads `[lint.templ]` from `hamr.toml`; flat `rule = "warning"|"error"|"off"` mapping; unknown rule IDs and invalid severities return an error
- `AllRuleIDs() []string`, `DefaultSeverity(id) Severity` — registry helpers (used by the `--rule` CLI flag)

---

## Optional (scaffold flag-gated)

### `e2e` (`--e2e`)
Reusable go-rod browser helpers for Playwright-free E2E testing.

- `SetupBrowser(t, opts...) *rod.Browser` — launches Chromium, auto-cleanup
- `NewPage(t, browser, url) *rod.Page` — navigates, screenshots on failure
- Actions: `Input`, `Click`, `SelectOption`
- Queries: `ElementExists`, `ElementNotExists`, `ElementText`, `ElementAttribute`, `ElementCount`
- Waits: `WaitForElement`, `WaitForURLChange`, `WaitForElementRemoved`
- HTMX-aware: `WaitForHTMXIdle`, `WaitForHTMXSwap`, `ClickAndWaitHTMX`
- Asserts: `AssertElementExists`, `AssertElementContainsText`, `AssertURL`, `AssertElementCount`, `AssertElementHasClass`, `AssertURLContains`
- Options: `WithHeadless`, `WithSlowMotion`, `WithTimeout`, `WithArtifactDir`

### `auth` (scaffold default)
Argon2id password hashing + session management.

- `HashPassword(password)`, `CheckPassword(password, encodedHash)`, `NeedsRehash(encodedHash)`
- `GenerateToken()`, `GenerateTokenN(n)`
- `NewSessionManager(store, opts...) *SessionManager`
- Session methods: `CreateSession`, `ValidateSession`, `DeleteSession`, `DeleteSubjectSessions`, `CookieName`, `Duration`
- `SessionStore` interface: `Create`, `GetByToken`, `Delete`, `DeleteBySubjectID`
- Options: `WithDuration`, `WithCookieName`, `WithCookiePath`, `WithCookieDomain`, `WithCookieSecure`, `WithSameSite`, `WithSlidingRefresh`

### `i18n` (`--locale`)
JSON translation files, CLDR plural rules, `text/template` interpolation.

- `NewBundle(cfg) (*Bundle, error)` — loads `locales/*.json`
- `Bundle.Translator(locale)`, `SupportedLocales()`, `HasLocale()`, `ResolveLocale()`
- `Translator.T(key, args...)` — translates with optional count/data
- `FromContext(c) *Translator`
- `DirectionFor(lang)` — `"ltr"` or `"rtl"`

### `storage` (`--storage local|s3`)
Pluggable file storage interface.

- `FileStorage` interface: `Save`, `Open`, `Delete`, `Exists`, `List`
- `SignableStorage` extends with `SignURL(ctx, path, expiry, opts...)` — `WithAttachment(filename)` signs a forced-download URL (Content-Disposition: attachment)
- `NewLocalStorage(basePath, opts...)`, `NewS3Storage(cfg, opts...)`

### `media` (on top of `storage`)
Image/video upload, processing, serving.

- `NewLocalImageStore(store, urlPrefix, cfg, opts...)` / `NewS3ImageStore(...)`
- `NewLocalVideoStore(store, urlPrefix, cfg, opts...)` / `NewS3VideoStore(...)`
- Upload entry points: `Upload(ctx, fileHeader)`, `UploadFromReader(ctx, r, size)`, `UploadFromReaderWithID(ctx, id, r, size, overwrite)` — `*WithID` requires the 36-char canonical UUID; `overwrite=false` returns `ErrIDExists` (best-effort precheck)
- `Delete(ctx, id)`, `GetMedia(id)`, `ServeHandler()`
- Video transcode (opt-in): set `VideoStoreConfig.Transcode VideoTranscodeOptions{Preset: "medium"}` (or any non-zero field) and every upload is re-encoded to H.264/AAC MP4 with `+faststart` before saving; zero-valued struct preserves the upload bytes verbatim. Transcoding is slow — call `Upload*` from a worker pool (`pkg/async.Group`).
- Sentinels: `ErrFileTooLarge`, `ErrUnknownType`, `ErrVideoTooLong`, `ErrInvalidID`, `ErrIDExists`, `ErrFFmpegNotFound`
- Preset sizes: `SizesAvatar`, `SizesCard`, `SizesIcon`, `SizeOriginal`

### `websocket` (`--websocket`)
WebSocket hub with session, subject, and room routing.

- `NewHub(opts...) *Hub`, `Hub.Handler() echo.HandlerFunc`, `Hub.Close()`
- Options: `WithSessionIDFunc`, `WithSubjectIDFunc`, `WithOnMessage`, `WithAcceptOptions`, `WithLogger`, `WithSendBufferSize(n)`
- Sending: `SendToSession`, `SendToSubject`, `SendToRoom`, `SendToRoomExcept`, `Broadcast`
- Rooms: `JoinRoom`, `LeaveRoom`
- Events: `NewEvent`, `NewHTMLEvent`, `NewOuterHTMLEvent`, `NewTriggerEvent`
- `NewEmitter(hub)` — typed event wrapper over `Hub`

### `fingerprint` (backing `hamr gen static`)
Content-based static asset fingerprinting (SHA-256, 12 hex chars).

- `Fingerprint(srcDir, distDir) (*Manifest, error)`
- `Clean(distDir) error`
- `Manifest.WriteGoManifest(path, pkg) error`
- `IsFingerprinted(path) bool`

### `sync` (backing `hamr sync`)
File-watching and S3-syncing for static assets.

- `SyncAll(ctx, store, dir) error`
- `WatchAndSync(ctx, store, dir) error`

---

## Picking the right package (quick lookup)

| Need                                          | Reach for                   |
|-----------------------------------------------|-----------------------------|
| Read config from env                          | `config`                    |
| Spin up the HTTP server                       | `server`                    |
| Render a templ component as a response        | `respond.HTML`              |
| Redirect from a handler                       | `respond.Redirect`          |
| Tell if request came from HTMX                | `htmx.IsHTMX`               |
| Trigger an HTMX event / swap override         | `htmx.Trigger`, `Reswap`    |
| Validate a form                               | `validate.NewForm + Field`  |
| Hash/check a password                         | `auth.HashPassword`         |
| Log with request context                      | `logging.FromContext`       |
| Type-safe Echo context keys                   | `ctx.NewKey`                |
| Typed pointer helpers                         | `ptr`                       |
| Connect to Postgres / run migrations          | `db.Connect`, `db.Migrate`  |
| Protect routes behind login                   | `middleware.RequireAuth`    |
| RBAC                                          | `middleware.RequireRoles`   |
| CSRF / CORS / rate limit / flash / audit      | `middleware` (same-named)   |
| Fire-and-forget goroutine (safely)            | `async.Fire`                |
| Concurrent fan-out with first-error cancel    | `async.All`                 |
| Cron jobs                                     | `janitor`                   |
| Upload an image/video                         | `media`                     |
| Save a file locally or to S3                  | `storage`                   |
| Live server→client push                       | `websocket`                 |
| Translate a string                            | `i18n.FromContext(c).T(...)` |
| Fingerprint static assets (programmatic)      | `fingerprint`               |
| Lint templates (programmatic)                 | `templint`                  |
