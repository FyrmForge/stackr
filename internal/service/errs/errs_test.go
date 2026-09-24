package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestIs(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("outer: %w", err) }
	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"refused with reason is ErrRefused", wrap(Refusedf("key has no org")), ErrRefused, true},
		{"bare refused", wrap(ErrRefused), ErrRefused, true},
		{"refused is not not-found", Refusedf("x"), ErrNotFound, false},
		{"not found", wrap(ErrNotFound), ErrNotFound, true},
		{"busy", wrap(ErrBusy), ErrBusy, true},
		{"conflict is not refused", Conflictf("x"), ErrRefused, false},
	}
	for _, tt := range tests {
		if got := errors.Is(tt.err, tt.target); got != tt.want {
			t.Errorf("%s: errors.Is = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAs(t *testing.T) {
	err := fmt.Errorf("saving tile: %w", Invalidf("replicas", "must be at least %d", 1))
	if v, ok := IsInvalid(err); !ok || v.Field != "replicas" || v.Error() != "replicas: must be at least 1" {
		t.Errorf("IsInvalid = %+v, %v", v, ok)
	}
	if _, ok := IsConflict(err); ok {
		t.Error("an Invalid read as a Conflict")
	}
	if v, ok := IsConflict(fmt.Errorf("x: %w", Conflictf("stack is config-managed"))); !ok ||
		v.Msg != "stack is config-managed" {
		t.Errorf("IsConflict = %+v, %v", v, ok)
	}
	if v, ok := IsUnset(fmt.Errorf("x: %w", Unset{Param: "db/password"})); !ok || v.Param != "db/password" {
		t.Errorf("IsUnset = %+v, %v", v, ok)
	}
	if v := (Invalid{Msg: "bad body"}); v.Error() != "bad body" {
		t.Errorf("Invalid without field = %q", v.Error())
	}
}
