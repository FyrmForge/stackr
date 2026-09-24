# Extract: `service/svcerr`, `service/scheduler`

Source: `service/svcerr/svcerr.go`, `service/scheduler/scheduler.go`, `service/scheduler/scheduler_test.go` (base `internal/stackrd/`)
Commit: c2423f0
Taken: the four-plus-one error vocabulary with its `errors.Is`/`As` bridges and its HTTP mapping table; the schedule reload service's two-tier error policy
Cut: the concrete `infra/jobs` / `infra/backup` dependencies and the `LoadSchedules` store reads behind them; the nil guard and its test
Cuts belong to: `service/internal/flow/jobs` and `service/internal/leaf/backup` (the schedule tables); nowhere (the nil guard — see notes)

## The row promises a cron runner. This row's files do not contain one.

`service/scheduler/` is 72 lines of nil-guard facade: it re-registers the two
cron tables after a write that changed them, by calling `LoadSchedules` on two
infra services. The cron parsing, the tick loop, the missed-tick policy and the
not-double-starting-a-running-schedule guard are all behind those two calls, in
`infra/jobs` and `infra/backup` — outside this row's read scope. Nothing below
is invented to fill that gap. See "Notes for the builder".

## `service/errs` (from `svcerr`)

Target names differ from the old ones; the old→new table is in the notes.

```go
// Package errs is the one error vocabulary the service layer speaks. The
// mapping to an HTTP status happens once, at the edge, in the handler
// middleware — never in a service and never in a handler.
//
//	NotFound   → 404  not yours, or not there; the two are one answer on
//	                  purpose, so a probe cannot tell them apart
//	Refused    → 403  yours, but at the wrong level (member vs admin)
//	Conflict   → 409  a draft org or a config-managed stack owns it
//	Invalid    → 400  the request is wrong; Field names which part
//	Busy       → 503  a dependency this build was started without (the job
//	                  runner, on an API-only process). Answering 500 there
//	                  would call an operator out of bed for a configuration
//	                  choice.
//
// It imports nothing but the standard library, so every layer above the store
// can speak it without pulling a dependency along.
package errs

import (
	"errors"
	"fmt"
)

// ErrNotFound and ErrRefused are sentinels: they carry no detail because
// neither answer is allowed to explain itself. Compare with errors.Is.
var (
	ErrNotFound = errors.New("not found")
	ErrRefused  = errors.New("refused")
	// ErrBusy is "this server cannot do that right now", not "you may not":
	// the caller should retry, not change the request.
	ErrBusy = errors.New("unavailable")
)

// Conflict is a refusal whose reason the user needs in order to act on it —
// which file owns the stack, which org is still a draft. The message is a
// whole sentence because both surfaces render it verbatim.
type Conflict struct{ Msg string }

func (e Conflict) Error() string { return e.Msg }

// Refused is a refusal that may say why. ErrRefused stays the bare one: use it
// whenever the reason would confirm something about a resource the caller
// should not know exists. Use this when it cannot — an API key missing a scope
// says nothing about what the scope would have reached.
type Refused struct{ Msg string }

func (e Refused) Error() string { return e.Msg }

// Is makes errors.Is(err, ErrRefused) true for a Refused as well, so a caller
// can ask "was this a 403" without knowing which of the two it got.
func (e Refused) Is(target error) bool { return target == ErrRefused }

// Refusedf builds a Refused.
func Refusedf(format string, a ...any) error {
	return Refused{Msg: fmt.Sprintf(format, a...)}
}

// Invalid is a rejected input. Field is the request key the caller can fix
// ("replicas", "basic_auth_password"), empty when the fault is the request as
// a whole. Msg is a whole sentence for the same reason Conflict's is.
type Invalid struct {
	Field string
	Msg   string
}

func (e Invalid) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return e.Field + ": " + e.Msg
}

// Invalidf builds an Invalid. The field may be "".
func Invalidf(field, format string, a ...any) error {
	return Invalid{Field: field, Msg: fmt.Sprintf(format, a...)}
}

// Conflictf builds a Conflict.
func Conflictf(format string, a ...any) error {
	return Conflict{Msg: fmt.Sprintf(format, a...)}
}

// IsInvalid, IsConflict unwrap through wrapping. Handlers use the mapper
// rather than these; they exist for services that need to react to their own
// callees (retry a validation, say) without string matching.
func IsInvalid(err error) (Invalid, bool) {
	var v Invalid
	return v, errors.As(err, &v)
}

func IsConflict(err error) (Conflict, bool) {
	var v Conflict
	return v, errors.As(err, &v)
}
```

The mapper itself is `handlers/middleware.HTTP` in the old tree and was not
read for this extract. The table in the package doc above is copied verbatim
from the old package doc and is the whole contract the mapper implements.

## `service/internal/flow/schedule` (from `scheduler`)

The only rule in the old file is the two-tier error policy, and it survives the
reshape. The rest was plumbing for two concrete infra services.

```go
// Package schedule re-registers the schedule tables after anything that
// changes them. Before it existed, 28 call sites each carried their own error
// policy (one returned 500, fourteen discarded, two warned), and four paths
// that cascade schedule rows forgot to reload at all — so a deleted stack's
// cron kept ticking against a missing tile.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// extract: schedule list now passed in. The old service held *jobs.Service and
// *backup.Service and called LoadSchedules on each, which reads the DB.
// extract: dropped the store read behind LoadSchedules, belongs in flow/jobs
// and leaf/backup — each owns its table and hands its loader here.
type Reload struct {
	Name string // "cron", "backups" — used in the log line only
	Load func(context.Context) error
}

// New takes one Reload per schedule table.
func New(r ...Reload) *Service { return &Service{reloads: r} }

type Service struct{ reloads []Reload }

// Reload re-registers every schedule table.
//
// It returns nothing on purpose. A reload failure must not fail the write that
// preceded it: the row is already committed, and the worst case is one stale
// entry that ticks and logs until the next reload. The boot path is the one
// caller that wants the error, and it uses Boot.
func (s *Service) Reload(ctx context.Context) {
	for _, r := range s.reloads {
		if err := r.Load(ctx); err != nil {
			slog.Error("reloading schedules failed", "table", r.Name, "error", err)
		}
	}
}

// Boot is the one caller that gets the errors: nothing is serving yet, so a
// schedule table that will not load is worth reporting rather than swallowing.
func (s *Service) Boot(ctx context.Context) error {
	var errs []error
	for _, r := range s.reloads {
		if err := r.Load(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}
```

Cuts against the old file:

- `// extract: dropped the *jobs.Service / *backup.Service fields, belongs in
  flow/jobs and leaf/backup` — a reload facade should not know which two tables
  exist, and a flow importing infra breaks the layering rule this row is
  filtered by.
- `// extract: dropped ReloadCron and ReloadBackups, belongs in the caller` —
  separate methods existed only because the two deps were fields. Every path
  that deletes a tile, an environment or a stack cascades rows out of both
  tables, so "reload both" was already what callers wanted.
- `// extract: dropped the nil guard and scheduler_test.go's TestNilSafe,
  belongs nowhere` — the guard existed because tests and the API router ran
  without one or both halves. `service.New` now builds every flow once (step 1,
  task 8), and an empty `reloads` slice is already a no-op.
- `// extract: dropped Boot's named returns, belongs nowhere` — `(cronErr,
  backupErr error)` does not generalise past two tables; `errors.Join` says the
  same thing and names each table.

## Notes for the builder

- **The cron runner is not here; do not guess it.** The row asks for parsing,
  the tick loop, the missed-tick policy and the not-double-starting guard. None
  are in `service/scheduler/`. Its import block names where they are:
  `infra/jobs` and `infra/backup`, reached only through `LoadSchedules(ctx)`.
  Those two need their own extract row before anyone writes a tick loop.
  `flow/jobs` (step 1, task 10) has the durable-queue half and no cron half.
- **The two-tier error policy is the thing worth keeping.** Post-write reload
  logs and swallows, because the row is already committed. Boot returns,
  because nothing is serving yet. Keep both or the reason for either goes.
- **Old → new error names:** `ErrNotFound`→`ErrNotFound`,
  `ErrForbidden`→`ErrRefused`, `Forbidden`/`Forbiddenf`→`Refused`/`Refusedf`,
  `ErrUnavailable`→`ErrBusy`, `Conflict`/`Invalid` (and their `f` builders)
  unchanged. Step 1 also names `Unset`; it is not in this row —
  `config/varref`'s `UnsetError` is where it comes from.
- **The 404-is-two-answers rule is load-bearing.** "Not yours" and "not there"
  return the same status so a probe cannot enumerate resources by the
  difference. Same reason bare `ErrRefused` sits next to `Refused`-with-reason.
- **No test came over.** `svcerr` had none; `scheduler_test.go` tested the nil
  guard that is cut. Write task 6's `errors.Is`/`As` tests fresh against the two
  bridges: `Refused.Is` answering `ErrRefused`, and `IsInvalid`/`IsConflict`
  finding a wrapped value.

Size: source 192 lines, extract 226 lines — kept Go 147, header and notes 79. The filtered count is the Go one (147 < 192); the `.md` is longer only because this row's file carries the format's header and builder notes plus the "the cron runner is not in these files" finding.
