// Package svcerr is the one error vocabulary the service layer speaks (D4 of
// docs/plans/service-extraction/03-refactor.md). Four kinds, and the mapping
// to an HTTP status happens once, at the edge, in
// handlers/middleware.HTTP — never in a service and never in a handler.
//
//	NotFound   → 404  not yours, or not there; the two are one answer on
//	                  purpose, so a probe cannot tell them apart
//	Forbidden  → 403  yours, but at the wrong level (member vs admin)
//	Conflict   → 409  a draft org or a config-managed stack owns it
//	Invalid    → 400  the request is wrong; Field names which part
//
// Plus one that D4 does not list, because D4 was written about refusals and
// this is not one: Unavailable → 503, a dependency this build was started
// without (the job runner, on an API-only process). Answering 500 there would
// call an operator out of bed for a configuration choice.
//
// It imports nothing but the standard library, so every layer above the store
// can speak it without pulling a dependency along.
package svcerr

import (
	"errors"
	"fmt"
)

// ErrNotFound and ErrForbidden are sentinels: they carry no detail because
// neither answer is allowed to explain itself. Compare with errors.Is.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	// ErrUnavailable is "this server cannot do that right now", not "you may
	// not": the caller should retry, not change the request.
	ErrUnavailable = errors.New("unavailable")
)

// Conflict is a refusal whose reason the user needs in order to act on it —
// which file owns the stack, which org is still a draft. The message is a
// whole sentence because both surfaces render it verbatim.
type Conflict struct{ Msg string }

func (e Conflict) Error() string { return e.Msg }

// Forbidden is a refusal that may say why. ErrForbidden stays the bare one:
// use it whenever the reason would confirm something about a resource the
// caller should not know exists. Use this when it cannot — an API key missing
// a scope says nothing about what the scope would have reached.
type Forbidden struct{ Msg string }

func (e Forbidden) Error() string { return e.Msg }

// Is makes errors.Is(err, ErrForbidden) true for a Forbidden as well, so a
// caller can ask "was this a 403" without knowing which of the two it got.
func (e Forbidden) Is(target error) bool { return target == ErrForbidden }

// Forbiddenf builds a Forbidden.
func Forbiddenf(format string, a ...any) error {
	return Forbidden{Msg: fmt.Sprintf(format, a...)}
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
