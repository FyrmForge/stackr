package components

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/a-h/templ"
)

// Refused is a refused save drawn again: the accordion row it was saved
// from opens, its fields keep what was typed, the reason sits under the
// field it names, or over the row when no drawn field takes it. The rows
// around it, and every summary, still show what is stored. A field that
// takes a secret never reads Vals.
type Refused struct {
	Row   string     // the accordion row the form sits in (its posted open)
	Field string     // the field the reason is about; "" = the row's
	Msg   string     // plain words, no field key
	Vals  url.Values // the posted form
	used  bool       // a field drew Msg
}

type refusedKey struct{}

// WithRefused carries r to the fields drawn under ctx; nil clears it.
func WithRefused(ctx context.Context, r *Refused) context.Context {
	return context.WithValue(ctx, refusedKey{}, r)
}

func refusedOf(ctx context.Context) *Refused {
	r, _ := ctx.Value(refusedKey{}).(*Refused)
	return r
}

// Kept is a field's value: what was typed when its save was refused, else
// stored.
func Kept(ctx context.Context, name, stored string) string {
	if r := refusedOf(ctx); r != nil && r.Vals.Has(name) {
		return r.Vals.Get(name)
	}
	return stored
}

// KeptOn is a checkbox's state. An unticked box posts nothing, so a form
// with boxes lists its drawn keys in a hidden "fields" (as the tile rows
// do): a box listed there and not posted was unticked.
func KeptOn(ctx context.Context, name string, stored bool) bool {
	r := refusedOf(ctx)
	switch {
	case r == nil:
		return stored
	case r.Vals.Has(name):
		return true
	case slices.Contains(strings.Fields(r.Vals.Get("fields")), name):
		return false
	}
	return stored
}

// KeptErr is a field's error: the refusal's reason when it names this
// field, else err.
func KeptErr(ctx context.Context, name, err string) string {
	if r := refusedOf(ctx); r != nil && r.Field != "" && r.Field == name && r.Msg != "" {
		r.used = true
		return r.Msg
	}
	return err
}

// RefusedHere is the refusal drawn under ctx, nil = none.
func RefusedHere(ctx context.Context) *Refused { return refusedOf(ctx) }

// accBody draws one row's body. The refused row draws with the refusal
// (and its reason over the body when no field took it); every other row
// draws as stored.
func accBody(key string, body templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := refusedOf(ctx)
		if r == nil || r.Row != key {
			if body == nil {
				return nil
			}
			return body.Render(WithRefused(ctx, nil), w)
		}
		var buf bytes.Buffer
		if body != nil {
			if err := body.Render(ctx, &buf); err != nil {
				return err
			}
		}
		if !r.used {
			if err := FormError(r.Msg).Render(ctx, w); err != nil {
				return err
			}
		}
		_, err := w.Write(buf.Bytes())
		return err
	})
}

// accOpen is the row open on load: the refused one, else open.
func accOpen(ctx context.Context, rows []AccRow, open string) string {
	if r := refusedOf(ctx); r != nil {
		for _, row := range rows {
			if row.Key == r.Row {
				return r.Row
			}
		}
	}
	return open
}
