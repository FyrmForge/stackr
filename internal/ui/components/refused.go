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
	// More is every field the save was refused on, by name, Field's too:
	// each field shows its own reason at once.
	More map[string]string
	used bool // a field drew Msg
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
	r := refusedOf(ctx)
	switch {
	case r == nil || name == "":
	case r.More[name] != "":
		r.used = r.used || name == r.Field
		return r.More[name]
	case r.Field == name && r.Msg != "":
		r.used = true
		return r.Msg
	}
	return err
}

// Invalid marks an input whose field shows err: aria-invalid, and
// aria-describedby naming its FieldError, so a screen reader reads why.
func Invalid(field, err string) templ.Attributes {
	if err == "" {
		return nil
	}
	return templ.Attributes{"aria-invalid": "true", "aria-describedby": "error-" + field}
}

// RefusedHere is the refusal drawn under ctx, nil = none.
func RefusedHere(ctx context.Context) *Refused { return refusedOf(ctx) }

// accBody draws one row's body. The refused row draws with the refusal
// (its reason after the form that posted when no field took it, else over
// the body); the open row of a saved drawer takes the note the same way
// (else under the body); every other row draws as stored.
func accBody(key string, open bool, body templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := refusedOf(ctx)
		here := r != nil && r.Row == key
		bctx := ctx
		if !here {
			bctx = WithRefused(ctx, nil)
		}
		var buf bytes.Buffer
		if body != nil {
			if err := body.Render(bctx, &buf); err != nil {
				return err
			}
		}
		html := buf.String()
		var words templ.Component
		top := false
		if here && !r.used {
			words, top = FormError(r.Msg), true
		} else if a, _ := ctx.Value(actedKey{}).(*Acted); !here && open && a != nil && !a.used && a.Path == "" && a.Note != "" {
			a.used = true
			words = Note(a.Note)
		}
		if words == nil {
			_, err := io.WriteString(w, html)
			return err
		}
		var nb bytes.Buffer
		if err := actedNote(words).Render(ctx, &nb); err != nil {
			return err
		}
		at, ok := postAt(html, postedOf(ctx), false)
		switch {
		case ok:
		case top:
			at = 0
		default:
			at = len(html)
		}
		_, err := io.WriteString(w, html[:at]+nb.String()+html[at:])
		return err
	})
}

// Answered draws its children with what a POST did (note) or why it was
// refused (refusal) in the Section whose form posted it (WithPosted's URI);
// when no Section claims it, over the children.
func Answered(note, refusal string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		children := templ.GetChildren(ctx)
		a := &Acted{Path: postedOf(ctx), Note: note, Err: refusal}
		var buf bytes.Buffer
		if err := children.Render(WithActed(templ.ClearChildren(ctx), a), &buf); err != nil {
			return err
		}
		if a.Unclaimed() {
			if err := FormError(refusal).Render(ctx, w); err != nil {
				return err
			}
			if refusal == "" && note != "" {
				if err := Note(note).Render(ctx, w); err != nil {
					return err
				}
			}
		}
		_, werr := w.Write(buf.Bytes())
		return werr
	})
}

type postedKey struct{}

// WithPosted carries the request URI a POST came to, so an accordion row
// puts what the answer says right after the form that posted it.
func WithPosted(ctx context.Context, uri string) context.Context {
	return context.WithValue(ctx, postedKey{}, uri)
}

func postedOf(ctx context.Context) string {
	s, _ := ctx.Value(postedKey{}).(string)
	return s
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

// posted: html holds a control posting to Path (postAt).
func (a *Acted) posted(html string) (at int, ok bool) {
	return postAt(html, a.Path, a.Below)
}

// postAt: html holds a control posting to path, a request URI. One with a
// query matches only the same query; one without matches any; below also
// takes a control posting under path. at is where an answer goes: right
// after the posting <form>, so it sits by its button; len(html) when the
// control is not a form.
func postAt(html, path string, below bool) (at int, ok bool) {
	if path == "" {
		return 0, false
	}
	attr := `hx-post="` + templ.EscapeString(path)
	anyQuery := !strings.Contains(path, "?")
	for off := 0; ; {
		i := strings.Index(html[off:], attr)
		if i < 0 {
			return len(html), ok
		}
		i += off
		off = i + len(attr)
		rest := html[off:]
		exact := strings.HasPrefix(rest, `"`)
		query := anyQuery && strings.HasPrefix(rest, "?")
		under := below && strings.HasPrefix(rest, "/")
		if !exact && !query && !under {
			continue
		}
		ok = true // a button posting there; a form posting there wins
		lt := strings.LastIndex(html[:i], "<")
		end := strings.Index(rest, "</form>")
		if lt >= 0 && strings.HasPrefix(html[lt:], "<form") && end >= 0 {
			return off + end + len("</form>"), true
		}
	}
}

// Section is v0's settings card (form.Section): a titled panel, its scope
// chip ("org", "stack") and one help line over the controls. Title "" drops
// the heading and the chip, for a card the page heading already names. The
// innermost card holding the form an answer came from shows its note or
// refusal under the controls, and only its fields take the refusal (a
// sibling card's field of the same name stays clean).
func Section(title, scope, help string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		a, _ := ctx.Value(actedKey{}).(*Acted)
		if a == nil || a.Path == "" {
			return section(title, scope, help, nil).Render(ctx, w)
		}
		if a.used {
			return section(title, scope, help, nil).Render(WithRefused(ctx, nil), w)
		}
		children := templ.GetChildren(ctx)
		var buf bytes.Buffer
		if err := children.Render(WithRefused(templ.ClearChildren(ctx), nil), &buf); err != nil {
			return err
		}
		html := buf.String()
		if at, ok := a.posted(html); ok && !a.used {
			a.used = true
			if a.Ref != nil { // draw it again, its fields taking the refusal
				buf.Reset()
				if err := children.Render(WithRefused(templ.ClearChildren(ctx), a.Ref), &buf); err != nil {
					return err
				}
				html = buf.String()
				at, _ = a.posted(html)
			}
			var note templ.Component
			if e := a.err(); e != "" {
				note = FormError(e)
			} else if a.Note != "" {
				note = Note(a.Note)
			}
			if note != nil {
				var nb bytes.Buffer
				if err := actedNote(note).Render(ctx, &nb); err != nil {
					return err
				}
				html = html[:at] + nb.String() + html[at:]
			}
		}
		return section(title, scope, help, nil).Render(templ.WithChildren(ctx, templ.Raw(html)), w)
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

// noted is a drawer's body with what a save did: under the accordion row
// it opened again (the row its form named), else over the body.
func noted(note string, body templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if note == "" {
			return body.Render(ctx, w)
		}
		a := &Acted{Note: note}
		var buf bytes.Buffer
		if err := body.Render(WithActed(ctx, a), &buf); err != nil {
			return err
		}
		if a.Unclaimed() {
			if err := Note(note).Render(ctx, w); err != nil {
				return err
			}
		}
		_, err := w.Write(buf.Bytes())
		return err
	})
}


