package setup

import (
	"context"
	"net/url"

	"github.com/a-h/templ"
)

// Refill is a refused save coming back on its step, in both wizards: what
// was posted, so the form shows it again (secrets too: they came from this
// browser), and the refusal, under the field it names. Action is the path
// the form posted to; a refusal naming no posted field shows over that form.
type Refill struct {
	Action string
	Form   url.Values
	Field  string
	Msg    string
}

type refillKey struct{}

// WithRefill puts a refused save on the context the step renders with.
func WithRefill(ctx context.Context, r Refill) context.Context {
	return context.WithValue(ctx, refillKey{}, r)
}

func refillOf(ctx context.Context) Refill {
	r, _ := ctx.Value(refillKey{}).(Refill)
	return r
}

// Refilled says whether ctx carries a refused save.
func Refilled(ctx context.Context) bool { return refillOf(ctx).Action != "" }

// val is what input name shows: what was posted on a refused save, else v.
func val(ctx context.Context, name, v string) string {
	if f := refillOf(ctx).Form; f.Has(name) {
		return f.Get(name)
	}
	return v
}

// ticked is checkbox name on the form posting to action: as posted on a
// refused save of that form (an unticked box posts nothing), else on.
func ticked(ctx context.Context, action, name string, on bool) bool {
	if r := refillOf(ctx); r.Action == action {
		return r.Form.Has(name)
	}
	return on
}

// picked is option value of select name: as posted on a refused save, else
// picked.
func picked(ctx context.Context, name, value string, on bool) bool {
	if f := refillOf(ctx).Form; f.Has(name) {
		return f.Get(name) == value
	}
	return on
}

// fieldMsg is the refusal under input name, "" for none.
func fieldMsg(ctx context.Context, name string) string {
	if r := refillOf(ctx); r.Field == name && r.Form.Has(name) {
		return r.Msg
	}
	return ""
}

// formMsg is the refusal over the form posting to action: one that names no
// field the form posted.
func formMsg(ctx context.Context, action string) string {
	r := refillOf(ctx)
	if r.Action != action || (r.Field != "" && r.Form.Has(r.Field)) {
		return ""
	}
	return r.Msg
}

// errID is the id of input name's message, for aria-describedby.
func errID(name string) string { return "error-" + name }

// invalid marks input name refused for screen readers, nil when it is not.
func invalid(ctx context.Context, name string) templ.Attributes {
	if fieldMsg(ctx, name) == "" {
		return nil
	}
	return templ.Attributes{"aria-invalid": "true", "aria-describedby": errID(name)}
}

// ShellID is the step's outer element: a refused save swaps the step back
// into it.
const ShellID = "setup-shell"
