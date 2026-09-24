// Package errs is the one error vocabulary the service layer speaks. The
// mapping to an HTTP status happens once, at the edge, in the middleware —
// never in a service and never in a handler. Nothing branches on a string.
//
//	ErrNotFound → 404  not yours, or not there; one answer on purpose, so a
//	                   probe cannot tell them apart
//	Refused     → 403  yours, but at the wrong level
//	Conflict    → 409  something else owns it; Msg says what
//	Invalid     → 400  the request is wrong; Field names which part
//	Unset       → 409  a param ref has no value yet (a job parks on it)
//	ErrBusy     → 503  this server cannot do that right now; retry
//
// It imports nothing but the standard library.
package errs

import (
	"errors"
	"fmt"
)

// Sentinels carry no detail: neither answer may explain itself.
var (
	ErrNotFound = errors.New("not found")
	ErrRefused  = errors.New("refused")
	ErrBusy     = errors.New("unavailable")
)

// Conflict is a refusal whose reason the user needs to act on. Msg is a whole
// sentence: both surfaces render it verbatim.
type Conflict struct{ Msg string }

func (e Conflict) Error() string { return e.Msg }

// Refused is a refusal that may say why. Use bare ErrRefused whenever the
// reason would confirm something the caller should not know exists.
type Refused struct{ Msg string }

func (e Refused) Error() string { return e.Msg }

// Is makes errors.Is(err, ErrRefused) true for a Refused as well.
func (e Refused) Is(target error) bool { return target == ErrRefused }

// Invalid is a rejected input. Field is the request key the caller can fix,
// empty when the fault is the request as a whole.
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

// Unset is a param ref with no value in any scope. A job that meets it waits
// on Param instead of failing.
type Unset struct{ Param string }

func (e Unset) Error() string { return "param " + e.Param + " is not set" }

func Refusedf(format string, a ...any) error  { return Refused{Msg: fmt.Sprintf(format, a...)} }
func Conflictf(format string, a ...any) error { return Conflict{Msg: fmt.Sprintf(format, a...)} }
func Invalidf(field, format string, a ...any) error {
	return Invalid{Field: field, Msg: fmt.Sprintf(format, a...)}
}

// IsInvalid, IsConflict and IsUnset unwrap through wrapping.
func IsInvalid(err error) (Invalid, bool) {
	var v Invalid
	return v, errors.As(err, &v)
}

func IsConflict(err error) (Conflict, bool) {
	var v Conflict
	return v, errors.As(err, &v)
}

func IsUnset(err error) (Unset, bool) {
	var v Unset
	return v, errors.As(err, &v)
}
