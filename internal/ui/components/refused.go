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

// errLead and errDetail split a refusal at its first newline: the plain
// sentence, then the provider's raw text.
func errLead(msg string) string {
	lead, _, _ := strings.Cut(msg, "\n")
	return lead
}

func errDetail(msg string) string {
	_, d, _ := strings.Cut(msg, "\n")
	return strings.TrimSpace(d)
}

// Acted is what a POST answered with, drawn in the Section card holding the
// form that posted it (Path, the request URI it posted to), so the words
// sit where the user is looking. Ref is the refusal a field may have taken
// instead. Unclaimed() is what no card drew, for the page to show on top.
type Acted struct {
	Path, Note, Err string
	Ref             *Refused
	Below           bool // a control posting under Path matches too
	used            bool
}

type actedKey struct{}

// WithActed carries a to the Section cards drawn under ctx.
func WithActed(ctx context.Context, a *Acted) context.Context {
	return context.WithValue(ctx, actedKey{}, a)
}

// err is the refusal unless a field already shows it.
func (a *Acted) err() string {
	if a.Ref != nil && a.Ref.used {
		return ""
	}
	return a.Err
}

// Unclaimed is true when there is something to say and no card said it.
func (a *Acted) Unclaimed() bool { return !a.used && (a.Note != "" || a.err() != "") }

// Words is the note or the refusal still to show ("" note when refused).
func (a *Acted) Words() (note, err string) { return a.Note, a.err() }

// posted: html holds a control posting to Path, a request URI. One with a
// query matches only the same query; one without matches any.
func (a *Acted) posted(html string) bool {
	at := `hx-post="` + templ.EscapeString(a.Path)
	anyQuery := !strings.Contains(a.Path, "?")
	for rest := html; ; {
		i := strings.Index(rest, at)
		if i < 0 {
			return false
		}
		rest = rest[i+len(at):]
		if strings.HasPrefix(rest, `"`) || (anyQuery && strings.HasPrefix(rest, "?")) || (a.Below && strings.HasPrefix(rest, "/")) {
			return true
		}
	}
}

// Section is v0's settings card (form.Section): a titled panel, its scope
// chip ("org", "stack") and one help line over the controls. Title "" drops
// the heading and the chip, for a card the page heading already names. The
// innermost card holding the form an answer came from shows its note or
// refusal under the controls.
func Section(title, scope, help string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		a, _ := ctx.Value(actedKey{}).(*Acted)
		if a == nil || a.used || a.Path == "" {
			return section(title, scope, help, nil).Render(ctx, w)
		}
		var buf bytes.Buffer
		if err := templ.GetChildren(ctx).Render(templ.ClearChildren(ctx), &buf); err != nil {
			return err
		}
		var note templ.Component
		if !a.used && a.posted(buf.String()) {
			a.used = true
			if e := a.err(); e != "" {
				note = FormError(e)
			} else if a.Note != "" {
				note = Note(a.Note)
			}
		}
		return section(title, scope, help, note).Render(templ.WithChildren(ctx, templ.Raw(buf.String())), w)
	})
}

// ErrText is a refusal's words: the plain lead line, and under it the
// provider's own text (after the first newline) as a faint detail line.
func ErrText(msg string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, templ.EscapeString(errLead(msg))); err != nil {
			return err
		}
		if d := errDetail(msg); d != "" {
			return errDetailLine(d).Render(ctx, w)
		}
		return nil
	})
}
